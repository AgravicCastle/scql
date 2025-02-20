// Copyright 2023 Ant Group Co., Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"

	"github.com/secretflow/scql/pkg/audit"
	"github.com/secretflow/scql/pkg/constant"
	"github.com/secretflow/scql/pkg/executor"
	"github.com/secretflow/scql/pkg/interpreter/optimizer"
	"github.com/secretflow/scql/pkg/interpreter/translator"
	"github.com/secretflow/scql/pkg/parser"
	"github.com/secretflow/scql/pkg/planner/core"
	"github.com/secretflow/scql/pkg/proto-gen/scql"
	"github.com/secretflow/scql/pkg/scdb/auth"
	"github.com/secretflow/scql/pkg/scdb/config"
	"github.com/secretflow/scql/pkg/scdb/storage"
	"github.com/secretflow/scql/pkg/util/logutil"
)

const (
	defaultCallbackQueueSize = 10
)

type App struct {
	config             *config.Config
	engineClient       executor.EngineClient
	storage            *gorm.DB
	sessions           *cache.Cache
	queryDoneChan      chan string
	partyAuthenticator *auth.PartyAuthenticator
}

type LogicalPlanInfo struct {
	lp          core.LogicalPlan
	issuer      string
	engineInfos *translator.EnginesInfo
	ccls        []*scql.SecurityConfig_ColumnControl
}

type ExecutionPlanInfo struct {
	parties []*scql.SessionStartParams_Party
	subDAGs []*optimizer.SubDAG
	graph   *translator.Graph
	attr    *translator.Attribute
}

func NewApp(conf *config.Config, storage *gorm.DB, engineClient executor.EngineClient) (*App, error) {
	app := &App{
		config:  conf,
		storage: storage,
		sessions: cache.New(time.Duration(conf.SessionExpireTime),
			time.Duration(conf.SessionCheckInterval)),
		queryDoneChan: make(chan string, defaultCallbackQueueSize),
	}
	app.startQueryJobFinishHandler()
	app.engineClient = engineClient
	app.partyAuthenticator = auth.NewPartyAuthenticator(conf.PartyAuth)
	return app, nil
}

func (app *App) Port() int {
	return app.config.Port
}

func (app *App) HealthHandler(c *gin.Context) {
	c.String(http.StatusOK, "ok")
}

func (app *App) notifyQueryJobDone(sid string) {
	go func() {
		app.queryDoneChan <- sid
	}()
}

// startQueryJobFinishHandler starts a background goroutine to handle finished query job
func (app *App) startQueryJobFinishHandler() {
	go func() {
		for {
			// quit the forloop when server is about to shutdown
			select {
			case sid := <-app.queryDoneChan:
				app.onQueryJobDone(sid)
			}
		}
	}()
}

func (app *App) getSession(sid string) (*session, bool) {
	value, exists := app.sessions.Get(sid)
	if !exists {
		return nil, false
	}
	if result, ok := value.(*session); ok {
		return result, exists
	}
	return nil, false
}

func (app *App) onQueryJobDone(sid string) {
	session, exists := app.getSession(sid)
	logEntry := &logutil.MonitorLogEntry{
		SessionID:  sid,
		ActionName: constant.ActionNameSCDBQueryJobDone,
	}
	if !exists {
		logEntry.Reason = constant.ReasonSessionNotFound
		logEntry.ErrorMsg = fmt.Sprintf("App.onQueryJobDone: sessionID=%s not found in sessions", sid)
		logrus.Error(logEntry)
		return
	}

	logEntry.RequestID = session.request.BizRequestId
	logEntry.CostTime = time.Since(session.createdAt)
	if session.queryResultCbURL == "" {
		logEntry.ErrorMsg = "Session queryResultCbURL is empty"
		logrus.Warn(logEntry)
		return
	}

	timeout := time.Duration(app.config.QueryResultCbTimeout) * time.Millisecond
	if err := invokeQueryResultCb(session.queryResultCbURL, session.result, timeout); err != nil {
		logEntry.Reason = constant.ReasonCallbackFrontendFail
		logEntry.ErrorMsg = fmt.Sprintf("Failed to request query result callback URL=%s: %v", session.queryResultCbURL, err)
		logrus.Error(logEntry)
		return
	}
	logrus.Info(logEntry)
}

func (app *App) compile(lpInfo *LogicalPlanInfo, s *session) (*ExecutionPlanInfo, error) {
	// translate logical plan to execution plan
	t, err := translator.NewTranslator(
		lpInfo.engineInfos,
		&scql.SecurityConfig{
			ColumnControlList: lpInfo.ccls,
		},
		lpInfo.issuer, &app.config.SecurityCompromise)
	if err != nil {
		return nil, fmt.Errorf("error when translating from logical plan to execution plan: %v", err)
	}
	ep, err := t.Translate(lpInfo.lp)
	if err != nil {
		return nil, err
	}
	s.GetSessionVars().AffectedByGroupThreshold = t.AffectedByGroupThreshold
	logrus.Infof("[Translator] execution plan: \n%s\n", ep.DumpGraphviz())

	graphChecker := translator.NewGraphChecker(ep)
	if err := graphChecker.Check(); err != nil {
		return nil, err
	}

	p := optimizer.NewGraphPartitioner(ep)
	p.NaivePartition()

	return &ExecutionPlanInfo{
		parties: s.parties,
		subDAGs: p.SubDAGs,
		graph:   p.Graph,
		attr:    &translator.Attribute{},
	}, nil
}

func (app *App) compilePrepare(ctx context.Context, s *session) (*LogicalPlanInfo, error) {
	// step1. find out issuer's party code
	issuer := s.GetSessionVars().User

	if issuer.Username == storage.DefaultRootName && issuer.Hostname == storage.DefaultHostName {
		return nil, fmt.Errorf("user root has no privilege to execute dql")
	}
	issuerPartyCode, err := storage.QueryUserPartyCode(s.GetSessionVars().Storage, issuer.Username, issuer.Hostname)
	if err != nil {
		return nil, fmt.Errorf("failed to query issuer party code: %v", err)
	}

	is, err := storage.QueryDBInfoSchema(s.GetSessionVars().Storage, s.request.DbName)
	if err != nil {
		return nil, err
	}

	p := parser.New()
	stmts, _, err := p.Parse(s.request.Query, "", "")
	if err != nil {
		return nil, err
	}

	// Ast -> logical plan
	lp, _, err := core.BuildLogicalPlanWithOptimization(
		ctx, s, stmts[0], is)
	if err != nil {
		return nil, fmt.Errorf("error when building logical plan: %v", err)
	}

	logrus.Debugf("[Translator] plan in one string: %s", core.ToString(lp))
	logrus.Debugf("[Translator] logical plan: \n%s\n", core.DrawLogicalPlan(lp))

	// step2. collect datasources which are referenced by the query via analyzing the logical plan
	dsList, err := datasourcesInDQL(s.request.Query, s.request.DbName, is)
	if err != nil {
		return nil, err
	}

	// check datasource
	if len(dsList) == 0 {
		return nil, fmt.Errorf("no datasource specified in the query")
	}
	dbName := dsList[0].DBName.String()
	s.GetSessionVars().CurrentDB = dbName
	// check if referenced tables are in the same db
	if len(dsList) > 1 {
		for _, ds := range dsList[1:] {
			if ds.DBName.String() != dbName {
				return nil, fmt.Errorf("query is not allowed to execute cross database")
			}
		}
	}

	// step3. collect referenced table schemas
	tableNames := make([]string, 0, len(dsList))
	for _, ds := range dsList {
		tableNames = append(tableNames, ds.TableInfo().Name.String())
	}
	tableSchemas, err := storage.QueryTableSchemas(s.GetSessionVars().Storage, dbName, tableNames)
	if err != nil {
		return nil, fmt.Errorf("failed to query referenced table schemas: %v", err)
	}

	// step4. collect participating engines which can be derived from tables referenced by the query
	engines, err := app.askEngineInfoByTables(s, dbName, tableNames)
	if err != nil {
		return nil, fmt.Errorf("failed to askEngineInfoByTables: %v", err)
	}

	// step5. collect needed ccl information
	partyOwners, err := storage.QueryTablesOwner(s.GetSessionVars().Storage, dbName, tableNames)
	if err != nil {
		return nil, fmt.Errorf("failed to query referenced table's owner: %v", err)
	}

	var ccl []*scql.SecurityConfig_ColumnControl
	foundQueryIssuer := false
	for _, partyOwner := range partyOwners {
		if partyOwner == issuer.Username {
			foundQueryIssuer = true
		}
		cc, err := collectCCLForUser(s.GetSessionVars().Storage, partyOwner, storage.DefaultHostName, tableSchemas)
		if err != nil {
			return nil, err
		}
		ccl = append(ccl, cc...)
	}
	// SecurityConfig should contains the query issuer
	if !foundQueryIssuer {
		cc, err := collectCCLForUser(s.GetSessionVars().Storage, issuer.Username, issuer.Hostname, tableSchemas)
		if err != nil {
			return nil, err
		}
		ccl = append(ccl, cc...)
	}

	logrus.Debugf("CCL: %v", ccl)
	audit.RecordSecurityConfig(&scql.SecurityConfig{ColumnControlList: ccl}, s.id)
	return &LogicalPlanInfo{
		lp:          lp,
		issuer:      issuerPartyCode,
		engineInfos: engines,
		ccls:        ccl,
	}, nil
}

func (app *App) DestroySession(id string, destroyReason string) {
	session, ok := app.getSession(id)
	logEntry := &logutil.MonitorLogEntry{
		SessionID:  id,
		ActionName: fmt.Sprintf("%v@%v", "SessionManager", "DestroySession"),
		Reason:     destroyReason,
	}
	if !ok || session == nil {
		logEntry.ErrorMsg = fmt.Sprintf("Session id:%v not found", id)
		logrus.Infof("%v|SessionExists:%v", logEntry, false)
		return
	}

	// Send end session task to all parties.
	engineErr := session.engineStub.EndSession(session.ctx,
		&scql.StopSessionRequest{
			SessionId: session.id,
		},
		session.partyInfo.GetParties())
	var errMsg string
	if engineErr != nil {
		errMsg = engineErr.Error()
	}
	// fall through here even when nil != err
	logEntry.ErrorMsg += errMsg
	logEntry.RequestID = session.request.BizRequestId
	logrus.Infof("%v|SessionExists:%v|EngineEndSessionOk:%v", logEntry, true, engineErr == nil)
}
