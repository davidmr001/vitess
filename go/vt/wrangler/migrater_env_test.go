/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package wrangler

import (
	"fmt"
	"testing"

	"golang.org/x/net/context"
	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/binlog/binlogplayer"
	"vitess.io/vitess/go/vt/key"
	"vitess.io/vitess/go/vt/logutil"
	binlogdatapb "vitess.io/vitess/go/vt/proto/binlogdata"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	"vitess.io/vitess/go/vt/proto/vschema"
	vschemapb "vitess.io/vitess/go/vt/proto/vschema"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/memorytopo"
	"vitess.io/vitess/go/vt/topotools"
	"vitess.io/vitess/go/vt/vttablet/tabletmanager/vreplication"
	"vitess.io/vitess/go/vt/vttablet/tmclient"
)

const vreplQueryks = "select id, source from _vt.vreplication where workflow='test' and db_name='vt_ks'"
const vreplQueryks2 = "select id, source from _vt.vreplication where workflow='test' and db_name='vt_ks2'"

type testMigraterEnv struct {
	ts              *topo.Server
	wr              *Wrangler
	sourceMasters   []*fakeTablet
	targetMasters   []*fakeTablet
	dbSourceClients []*fakeDBClient
	dbTargetClients []*fakeDBClient
	allDBClients    []*fakeDBClient
	targetKeyspace  string
}

// testShardMigraterEnv has some convenience functions for adding expected queries.
// They are approximate and should be only used to test other features like stream migration.
// Use explicit queries for testing the actual shard migration.
type testShardMigraterEnv struct {
	testMigraterEnv
	sourceShards, targetShards       []string
	sourceKeyRanges, targetKeyRanges []*topodatapb.KeyRange
}

func newTestTableMigrater(ctx context.Context, t *testing.T) *testMigraterEnv {
	tme := &testMigraterEnv{}
	tme.ts = memorytopo.NewServer("cell1", "cell2")
	tme.wr = New(logutil.NewConsoleLogger(), tme.ts, tmclient.NewTabletManagerClient())

	sourceShards := []string{"-40", "40-"}
	targetShards := []string{"-80", "80-"}

	tabletID := 10
	for _, shard := range sourceShards {
		tme.sourceMasters = append(tme.sourceMasters, newFakeTablet(t, tme.wr, "cell1", uint32(tabletID), topodatapb.TabletType_MASTER, nil, TabletKeyspaceShard(t, "ks1", shard)))
		tabletID += 10
	}
	for _, shard := range targetShards {
		tme.targetMasters = append(tme.targetMasters, newFakeTablet(t, tme.wr, "cell1", uint32(tabletID), topodatapb.TabletType_MASTER, nil, TabletKeyspaceShard(t, "ks2", shard)))
		tabletID += 10
	}

	vs := &vschemapb.Keyspace{
		Sharded: true,
		Vindexes: map[string]*vschemapb.Vindex{
			"hash": {
				Type: "hash",
			},
		},
		Tables: map[string]*vschemapb.Table{
			"t1": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "c1",
					Name:   "hash",
				}},
			},
			"t2": {
				ColumnVindexes: []*vschemapb.ColumnVindex{{
					Column: "c1",
					Name:   "hash",
				}},
			},
		},
	}
	if err := tme.ts.SaveVSchema(ctx, "ks1", vs); err != nil {
		t.Fatal(err)
	}
	if err := tme.ts.SaveVSchema(ctx, "ks2", vs); err != nil {
		t.Fatal(err)
	}
	if err := tme.ts.RebuildSrvVSchema(ctx, nil); err != nil {
		t.Fatal(err)
	}
	err := topotools.RebuildKeyspace(ctx, logutil.NewConsoleLogger(), tme.ts, "ks1", []string{"cell1"})
	if err != nil {
		t.Fatal(err)
	}
	err = topotools.RebuildKeyspace(ctx, logutil.NewConsoleLogger(), tme.ts, "ks2", []string{"cell1"})
	if err != nil {
		t.Fatal(err)
	}

	tme.startTablets(t)
	tme.createDBClients(ctx, t)
	tme.setMasterPositions()

	for i, targetShard := range targetShards {
		var rows []string
		for j, sourceShard := range sourceShards {
			bls := &binlogdatapb.BinlogSource{
				Keyspace: "ks1",
				Shard:    sourceShard,
				Filter: &binlogdatapb.Filter{
					Rules: []*binlogdatapb.Rule{{
						Match:  "t1",
						Filter: fmt.Sprintf("select * from t1 where in_keyrange('%s')", targetShard),
					}, {
						Match:  "t2",
						Filter: fmt.Sprintf("select * from t2 where in_keyrange('%s')", targetShard),
					}},
				},
			}
			rows = append(rows, fmt.Sprintf("%d|%v", j+1, bls))
		}
		tme.dbTargetClients[i].addInvariant(vreplQueryks2, sqltypes.MakeTestResult(sqltypes.MakeTestFields(
			"id|source",
			"int64|varchar"),
			rows...),
		)
	}

	if err := tme.wr.saveRoutingRules(ctx, map[string][]string{
		"t1":     {"ks1.t1"},
		"ks2.t1": {"ks1.t1"},
		"t2":     {"ks1.t2"},
		"ks2.t2": {"ks1.t2"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tme.ts.RebuildSrvVSchema(ctx, nil); err != nil {
		t.Fatal(err)
	}

	tme.targetKeyspace = "ks2"
	return tme
}

func newTestShardMigrater(ctx context.Context, t *testing.T, sourceShards, targetShards []string) *testShardMigraterEnv {
	tme := &testShardMigraterEnv{}
	tme.ts = memorytopo.NewServer("cell1", "cell2")
	tme.wr = New(logutil.NewConsoleLogger(), tme.ts, tmclient.NewTabletManagerClient())
	tme.sourceShards = sourceShards
	tme.targetShards = targetShards

	tabletID := 10
	for _, shard := range sourceShards {
		tme.sourceMasters = append(tme.sourceMasters, newFakeTablet(t, tme.wr, "cell1", uint32(tabletID), topodatapb.TabletType_MASTER, nil, TabletKeyspaceShard(t, "ks", shard)))
		tabletID += 10

		_, sourceKeyRange, err := topo.ValidateShardName(shard)
		if err != nil {
			t.Fatal(err)
		}
		if sourceKeyRange == nil {
			sourceKeyRange = &topodatapb.KeyRange{}
		}
		tme.sourceKeyRanges = append(tme.sourceKeyRanges, sourceKeyRange)
	}
	for _, shard := range targetShards {
		tme.targetMasters = append(tme.targetMasters, newFakeTablet(t, tme.wr, "cell1", uint32(tabletID), topodatapb.TabletType_MASTER, nil, TabletKeyspaceShard(t, "ks", shard)))
		tabletID += 10

		_, targetKeyRange, err := topo.ValidateShardName(shard)
		if err != nil {
			t.Fatal(err)
		}
		if targetKeyRange == nil {
			targetKeyRange = &topodatapb.KeyRange{}
		}
		tme.targetKeyRanges = append(tme.targetKeyRanges, targetKeyRange)
	}

	vs := &vschemapb.Keyspace{
		Sharded: true,
		Vindexes: map[string]*vschema.Vindex{
			"thash": {
				Type: "hash",
			},
		},
		Tables: map[string]*vschema.Table{
			"t1": {
				ColumnVindexes: []*vschema.ColumnVindex{{
					Columns: []string{"c1"},
					Name:    "thash",
				}},
			},
			"t2": {
				ColumnVindexes: []*vschema.ColumnVindex{{
					Columns: []string{"c1"},
					Name:    "thash",
				}},
			},
			"t3": {
				ColumnVindexes: []*vschema.ColumnVindex{{
					Columns: []string{"c1"},
					Name:    "thash",
				}},
			},
		},
	}
	if err := tme.ts.SaveVSchema(ctx, "ks", vs); err != nil {
		t.Fatal(err)
	}
	if err := tme.ts.RebuildSrvVSchema(ctx, nil); err != nil {
		t.Fatal(err)
	}
	err := topotools.RebuildKeyspace(ctx, logutil.NewConsoleLogger(), tme.ts, "ks", nil)
	if err != nil {
		t.Fatal(err)
	}

	tme.startTablets(t)
	tme.createDBClients(ctx, t)
	tme.setMasterPositions()

	for i, targetShard := range targetShards {
		var rows []string
		for j, sourceShard := range sourceShards {
			if !key.KeyRangesIntersect(tme.targetKeyRanges[i], tme.sourceKeyRanges[j]) {
				continue
			}
			bls := &binlogdatapb.BinlogSource{
				Keyspace: "ks",
				Shard:    sourceShard,
				Filter: &binlogdatapb.Filter{
					Rules: []*binlogdatapb.Rule{{
						Match:  "/.*",
						Filter: targetShard,
					}},
				},
			}
			rows = append(rows, fmt.Sprintf("%d|%v", j+1, bls))
		}
		tme.dbTargetClients[i].addInvariant(vreplQueryks, sqltypes.MakeTestResult(sqltypes.MakeTestFields(
			"id|source",
			"int64|varchar"),
			rows...),
		)
	}

	tme.targetKeyspace = "ks"
	for _, dbclient := range tme.dbSourceClients {
		dbclient.addInvariant(vreplQueryks, &sqltypes.Result{})
	}
	return tme
}

func (tme *testMigraterEnv) startTablets(t *testing.T) {
	for _, master := range tme.sourceMasters {
		master.StartActionLoop(t, tme.wr)
	}
	for _, master := range tme.targetMasters {
		master.StartActionLoop(t, tme.wr)
	}
}

func (tme *testMigraterEnv) stopTablets(t *testing.T) {
	for _, master := range tme.sourceMasters {
		master.StopActionLoop(t)
	}
	for _, master := range tme.targetMasters {
		master.StopActionLoop(t)
	}
}

func (tme *testMigraterEnv) createDBClients(ctx context.Context, t *testing.T) {
	for _, master := range tme.sourceMasters {
		dbclient := newFakeDBClient()
		tme.dbSourceClients = append(tme.dbSourceClients, dbclient)
		dbClientFactory := func() binlogplayer.DBClient { return dbclient }
		master.Agent.VREngine = vreplication.NewEngine(tme.ts, "", master.FakeMysqlDaemon, dbClientFactory, dbclient.DBName())
		if err := master.Agent.VREngine.Open(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, master := range tme.targetMasters {
		dbclient := newFakeDBClient()
		tme.dbTargetClients = append(tme.dbTargetClients, dbclient)
		dbClientFactory := func() binlogplayer.DBClient { return dbclient }
		master.Agent.VREngine = vreplication.NewEngine(tme.ts, "", master.FakeMysqlDaemon, dbClientFactory, dbclient.DBName())
		if err := master.Agent.VREngine.Open(ctx); err != nil {
			t.Fatal(err)
		}
	}
	tme.allDBClients = append(tme.dbSourceClients, tme.dbTargetClients...)
}

func (tme *testMigraterEnv) setMasterPositions() {
	for _, master := range tme.sourceMasters {
		master.FakeMysqlDaemon.CurrentMasterPosition = mysql.Position{
			GTIDSet: mysql.MariadbGTIDSet{
				mysql.MariadbGTID{
					Domain:   5,
					Server:   456,
					Sequence: 892,
				},
			},
		}
	}
	for _, master := range tme.targetMasters {
		master.FakeMysqlDaemon.CurrentMasterPosition = mysql.Position{
			GTIDSet: mysql.MariadbGTIDSet{
				mysql.MariadbGTID{
					Domain:   5,
					Server:   456,
					Sequence: 893,
				},
			},
		}
	}
}

func (tme *testShardMigraterEnv) forAllStreams(f func(i, j int)) {
	for i := range tme.targetShards {
		for j := range tme.sourceShards {
			if !key.KeyRangesIntersect(tme.targetKeyRanges[i], tme.sourceKeyRanges[j]) {
				continue
			}
			f(i, j)
		}
	}
}

func (tme *testShardMigraterEnv) expectCheckJournals() {
	for _, dbclient := range tme.dbSourceClients {
		dbclient.addQueryRE("select val from _vt.resharding_journal where id=.*", &sqltypes.Result{}, nil)
	}
}

func (tme *testShardMigraterEnv) expectWaitForCatchup() {
	state := sqltypes.MakeTestResult(sqltypes.MakeTestFields(
		"pos|state|message",
		"varchar|varchar|varchar"),
		"MariaDB/5-456-892|Running",
	)
	tme.forAllStreams(func(i, j int) {
		tme.dbTargetClients[i].addQuery(fmt.Sprintf("select pos, state, message from _vt.vreplication where id=%d", j+1), state, nil)

		// mi.waitForCatchup-> mi.wr.tmc.VReplicationExec('stopped for cutover')
		tme.dbTargetClients[i].addQuery(fmt.Sprintf("select id from _vt.vreplication where id = %d", j+1), &sqltypes.Result{Rows: [][]sqltypes.Value{{sqltypes.NewInt64(int64(j + 1))}}}, nil)
		tme.dbTargetClients[i].addQuery(fmt.Sprintf("update _vt.vreplication set state = 'Stopped', message = 'stopped for cutover' where id in (%d)", j+1), &sqltypes.Result{}, nil)
		tme.dbTargetClients[i].addQuery(fmt.Sprintf("select * from _vt.vreplication where id = %d", j+1), stoppedResult(j+1), nil)
	})
}

func (tme *testShardMigraterEnv) expectCreateJournals() {
	for _, dbclient := range tme.dbSourceClients {
		dbclient.addQueryRE("insert into _vt.resharding_journal.*", &sqltypes.Result{}, nil)
	}
}

func (tme *testShardMigraterEnv) expectCreateReverseReplication() {
	tme.forAllStreams(func(i, j int) {
		tme.dbSourceClients[j].addQueryRE(fmt.Sprintf("insert into _vt.vreplication.*%s.*%s.*MariaDB/5-456-893.*Stopped", tme.targetShards[i], tme.sourceShards[j]), &sqltypes.Result{InsertID: uint64(j + 1)}, nil)
		tme.dbSourceClients[j].addQuery(fmt.Sprintf("select * from _vt.vreplication where id = %d", j+1), stoppedResult(j+1), nil)
	})
}

func (tme *testShardMigraterEnv) expectDeleteTargetVReplication() {
	// NOTE: this is not a faithful reproduction of what should happen.
	// The ids returned are not accurate.
	for _, dbclient := range tme.dbTargetClients {
		dbclient.addQuery("select id from _vt.vreplication where db_name = 'vt_ks' and workflow = 'test'", resultid12, nil)
		dbclient.addQuery("delete from _vt.vreplication where id in (1, 2)", &sqltypes.Result{}, nil)
		dbclient.addQuery("delete from _vt.copy_state where vrepl_id in (1, 2)", &sqltypes.Result{}, nil)
	}
}

func (tme *testShardMigraterEnv) expectCancelMigration() {
	for _, dbclient := range tme.dbTargetClients {
		dbclient.addQuery("select id from _vt.vreplication where db_name = 'vt_ks' and workflow = 'test'", &sqltypes.Result{}, nil)
	}
}
