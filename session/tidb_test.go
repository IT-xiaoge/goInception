// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package session

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/pingcap/check"
	"github.com/hanchuanchuan/tidb/ast"
	"github.com/hanchuanchuan/tidb/domain"
	"github.com/hanchuanchuan/tidb/kv"
	"github.com/hanchuanchuan/tidb/store/mockstore"
	"github.com/hanchuanchuan/tidb/types"
	"github.com/hanchuanchuan/tidb/util"
	"github.com/hanchuanchuan/tidb/util/auth"
	"github.com/hanchuanchuan/tidb/util/logutil"
	"github.com/hanchuanchuan/tidb/util/testleak"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

func TestT(t *testing.T) {
	logLevel := os.Getenv("log_level")
	logutil.InitLogger(&logutil.LogConfig{
		Level: logLevel,
	})
	CustomVerboseFlag = true
	TestingT(t)
}

var _ = Suite(&testMainSuite{})

type testMainSuite struct {
	dbName string
	store  kv.Storage
	dom    *domain.Domain
}

type brokenStore struct{}

func (s *brokenStore) Open(schema string) (kv.Storage, error) {
	return nil, errors.New("try again later")
}

func (s *testMainSuite) SetUpSuite(c *C) {
	testleak.BeforeTest()
	s.dbName = "test_main_db"
	s.store = newStore(c, s.dbName)
	dom, err := BootstrapSession(s.store)
	c.Assert(err, IsNil)
	s.dom = dom
}

func (s *testMainSuite) TearDownSuite(c *C) {
	defer testleak.AfterTest(c)()
	s.dom.Close()
	err := s.store.Close()
	c.Assert(err, IsNil)
	removeStore(c, s.dbName)
}

// Testcase for arg type.
func (s *testMainSuite) TestCheckArgs(c *C) {
	checkArgs(nil, true, false, int8(1), int16(1), int32(1), int64(1), 1,
		uint8(1), uint16(1), uint32(1), uint64(1), uint(1), float32(1), float64(1),
		"abc", []byte("abc"), time.Now(), time.Hour, time.Local)
}

func (s *testMainSuite) TestIsQuery(c *C) {
	tbl := []struct {
		sql string
		ok  bool
	}{
		{"/*comment*/ select 1;", true},
		{"/*comment*/ /*comment*/ select 1;", true},
		{"select /*comment*/ 1 /*comment*/;", true},
		{"(select /*comment*/ 1 /*comment*/);", true},
	}
	for _, t := range tbl {
		c.Assert(IsQuery(t.sql), Equals, t.ok, Commentf(t.sql))
	}
}

func (s *testMainSuite) TestTrimSQL(c *C) {
	tbl := []struct {
		sql    string
		target string
	}{
		{"/*comment*/ select 1; ", "select 1;"},
		{"/*comment*/ /*comment*/ select 1;", "select 1;"},
		{"select /*comment*/ 1 /*comment*/;", "select /*comment*/ 1 /*comment*/;"},
		{"/*comment select 1; ", "/*comment select 1;"},
	}
	for _, t := range tbl {
		c.Assert(trimSQL(t.sql), Equals, t.target, Commentf(t.sql))
	}
}

func (s *testMainSuite) TestRetryOpenStore(c *C) {
	begin := time.Now()
	RegisterStore("dummy", &brokenStore{})
	store, err := newStoreWithRetry("dummy://dummy-store", 3)
	if store != nil {
		defer store.Close()
	}
	c.Assert(err, NotNil)
	elapse := time.Since(begin)
	c.Assert(uint64(elapse), GreaterEqual, uint64(3*time.Second))
}

func (s *testMainSuite) TestRetryDialPumpClient(c *C) {
	retryDialPumpClientMustFail := func(binlogSocket string, clientCon *grpc.ClientConn, maxRetries int, dialerOpt grpc.DialOption) (err error) {
		return util.RunWithRetry(maxRetries, 10, func() (bool, error) {
			// Assume that it'll always return an error.
			return true, errors.New("must fail")
		})
	}
	begin := time.Now()
	err := retryDialPumpClientMustFail("", nil, 3, nil)
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "must fail")
	elapse := time.Since(begin)
	c.Assert(uint64(elapse), GreaterEqual, uint64(6*10*time.Millisecond))
}

func (s *testMainSuite) TestSysSessionPoolGoroutineLeak(c *C) {
	store, dom := newStoreWithBootstrap(c, s.dbName+"goroutine_leak")
	defer dom.Close()
	defer store.Close()
	se, err := createSession(store)
	c.Assert(err, IsNil)

	// Test an issue that sysSessionPool doesn't call session's Close, cause
	// asyncGetTSWorker goroutine leak.
	count := 200
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func(se *session) {
			_, _, err := se.ExecRestrictedSQL(se, "select * from mysql.user limit 1")
			c.Assert(err, IsNil)
			wg.Done()
		}(se)
	}
	wg.Wait()
	se.sysSessionPool().Close()
	c.Assert(se.sysSessionPool().IsClosed(), Equals, true)
}

func newStore(c *C, dbPath string) kv.Storage {
	store, err := mockstore.NewMockTikvStore()
	c.Assert(err, IsNil)
	return store
}

func newStoreWithBootstrap(c *C, dbPath string) (kv.Storage, *domain.Domain) {
	store, err := mockstore.NewMockTikvStore()
	c.Assert(err, IsNil)
	dom, err := BootstrapSession(store)
	c.Assert(err, IsNil)
	return store, dom
}

var testConnID uint64

func newSession(c *C, store kv.Storage, dbName string) Session {
	se, err := CreateSession4Test(store)
	id := atomic.AddUint64(&testConnID, 1)
	se.SetConnectionID(id)
	c.Assert(err, IsNil)
	se.Auth(&auth.UserIdentity{Username: "root", Hostname: `%`}, nil, []byte("012345678901234567890"))
	mustExecSQL(c, se, "create database if not exists "+dbName)
	mustExecSQL(c, se, "use "+dbName)
	return se
}

func removeStore(c *C, dbPath string) {
	os.RemoveAll(dbPath)
}

func exec(se Session, sql string, args ...interface{}) (ast.RecordSet, error) {
	ctx := context.Background()
	if len(args) == 0 {
		rs, err := se.Execute(ctx, sql)
		if err == nil && len(rs) > 0 {
			return rs[0], nil
		}
		return nil, err
	}
	stmtID, _, _, err := se.PrepareStmt(sql)
	if err != nil {
		return nil, err
	}
	rs, err := se.ExecutePreparedStmt(ctx, stmtID, args...)
	if err != nil {
		return nil, err
	}
	return rs, nil
}

func mustExecSQL(c *C, se Session, sql string, args ...interface{}) ast.RecordSet {
	rs, err := exec(se, sql, args...)
	c.Assert(err, IsNil)
	return rs
}

func match(c *C, row []types.Datum, expected ...interface{}) {
	c.Assert(len(row), Equals, len(expected))
	for i := range row {
		got := fmt.Sprintf("%v", row[i].GetValue())
		need := fmt.Sprintf("%v", expected[i])
		c.Assert(got, Equals, need)
	}
}