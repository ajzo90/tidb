// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package ddl

import (
	"context"
	"testing"

	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/util/dbterror"
	"github.com/stretchr/testify/require"
)

func testPlacementPolicyInfo(t *testing.T, d *ddl, name string, settings *model.PlacementSettings) *model.PolicyInfo {
	policy := &model.PolicyInfo{
		Name:              model.NewCIStr(name),
		PlacementSettings: settings,
	}
	genIDs, err := d.genGlobalIDs(1)
	require.NoError(t, err)
	policy.ID = genIDs[0]
	return policy
}

func testCreatePlacementPolicy(t *testing.T, ctx sessionctx.Context, d *ddl, policyInfo *model.PolicyInfo) *model.Job {
	job := &model.Job{
		SchemaName: policyInfo.Name.L,
		Type:       model.ActionCreatePlacementPolicy,
		BinlogInfo: &model.HistoryInfo{},
		Args:       []interface{}{policyInfo},
	}
	err := d.doDDLJob(ctx, job)
	require.NoError(t, err)

	v := getSchemaVer(t, ctx)
	policyInfo.State = model.StatePublic
	checkHistoryJobArgs(t, ctx, job.ID, &historyJobArgs{ver: v})
	policyInfo.State = model.StateNone
	return job
}

func TestPlacementPolicyInUse(t *testing.T) {
	store := createMockStore(t)
	defer func() {
		require.NoError(t, store.Close())
	}()

	ctx := context.Background()
	d, err := testNewDDLAndStart(ctx, WithStore(store))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, d.Stop())
	}()
	sctx := testNewContext(d)

	db1, err := testSchemaInfo(d, "db1")
	require.NoError(t, err)
	testCreateSchema(t, sctx, d, db1)
	db1.State = model.StatePublic

	db2, err := testSchemaInfo(d, "db2")
	require.NoError(t, err)
	testCreateSchema(t, sctx, d, db2)
	db2.State = model.StatePublic

	policySettings := &model.PlacementSettings{PrimaryRegion: "r1", Regions: "r1,r2"}
	p1 := testPlacementPolicyInfo(t, d, "p1", policySettings)
	p2 := testPlacementPolicyInfo(t, d, "p2", policySettings)
	p3 := testPlacementPolicyInfo(t, d, "p3", policySettings)
	p4 := testPlacementPolicyInfo(t, d, "p4", policySettings)
	p5 := testPlacementPolicyInfo(t, d, "p5", policySettings)
	testCreatePlacementPolicy(t, sctx, d, p1)
	testCreatePlacementPolicy(t, sctx, d, p2)
	testCreatePlacementPolicy(t, sctx, d, p3)
	testCreatePlacementPolicy(t, sctx, d, p4)
	testCreatePlacementPolicy(t, sctx, d, p5)

	t1, err := testTableInfo(d, "t1", 1)
	require.NoError(t, err)
	t1.PlacementPolicyRef = &model.PolicyRefInfo{ID: p1.ID, Name: p1.Name}
	testCreateTable(t, sctx, d, db1, t1)
	t1.State = model.StatePublic
	db1.Tables = append(db1.Tables, t1)

	t2, err := testTableInfo(d, "t2", 1)
	require.NoError(t, err)
	t2.PlacementPolicyRef = &model.PolicyRefInfo{ID: p1.ID, Name: p1.Name}
	testCreateTable(t, sctx, d, db2, t2)
	t2.State = model.StatePublic
	db2.Tables = append(db2.Tables, t2)

	t3, err := testTableInfo(d, "t3", 1)
	require.NoError(t, err)
	t3.PlacementPolicyRef = &model.PolicyRefInfo{ID: p2.ID, Name: p2.Name}
	testCreateTable(t, sctx, d, db1, t3)
	t3.State = model.StatePublic
	db1.Tables = append(db1.Tables, t3)

	dbP, err := testSchemaInfo(d, "db_p")
	require.NoError(t, err)
	dbP.PlacementPolicyRef = &model.PolicyRefInfo{ID: p4.ID, Name: p4.Name}
	dbP.State = model.StatePublic
	testCreateSchema(t, sctx, d, dbP)

	t4 := testTableInfoWithPartition(t, d, "t4", 1)
	t4.Partition.Definitions[0].PlacementPolicyRef = &model.PolicyRefInfo{ID: p5.ID, Name: p5.Name}
	testCreateTable(t, sctx, d, db1, t4)
	t4.State = model.StatePublic
	db1.Tables = append(db1.Tables, t4)

	builder, err := infoschema.NewBuilder(store, nil).InitWithDBInfos(
		[]*model.DBInfo{db1, db2, dbP},
		nil,
		[]*model.PolicyInfo{p1, p2, p3, p4, p5},
		1,
	)
	require.NoError(t, err)
	is := builder.Build()

	for _, policy := range []*model.PolicyInfo{p1, p2, p4, p5} {
		require.True(t, dbterror.ErrPlacementPolicyInUse.Equal(checkPlacementPolicyNotInUseFromInfoSchema(is, policy)))
		require.NoError(t, kv.RunInNewTxn(ctx, sctx.GetStore(), false, func(ctx context.Context, txn kv.Transaction) error {
			m := meta.NewMeta(txn)
			require.True(t, dbterror.ErrPlacementPolicyInUse.Equal(checkPlacementPolicyNotInUseFromMeta(m, policy)))
			return nil
		}))
	}

	require.NoError(t, checkPlacementPolicyNotInUseFromInfoSchema(is, p3))
	require.NoError(t, kv.RunInNewTxn(ctx, sctx.GetStore(), false, func(ctx context.Context, txn kv.Transaction) error {
		m := meta.NewMeta(txn)
		require.NoError(t, checkPlacementPolicyNotInUseFromMeta(m, p3))
		return nil
	}))
}
