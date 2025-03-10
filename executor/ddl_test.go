// Copyright 2016 PingCAP, Inc.
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

package executor_test

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingcap/failpoint"
	ddltestutil "github.com/pingcap/tidb/ddl/testutil"
	ddlutil "github.com/pingcap/tidb/ddl/util"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/errno"
	"github.com/pingcap/tidb/executor"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/parser/terror"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/table/tables"
	"github.com/pingcap/tidb/testkit"
	"github.com/pingcap/tidb/testkit/testutil"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/dbterror"
	"github.com/stretchr/testify/require"
)

func TestTruncateTable(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec(`drop table if exists truncate_test;`)
	tk.MustExec(`create table truncate_test (a int)`)
	tk.MustExec(`insert truncate_test values (1),(2),(3)`)
	result := tk.MustQuery("select * from truncate_test")
	result.Check(testkit.Rows("1", "2", "3"))
	tk.MustExec("truncate table truncate_test")
	result = tk.MustQuery("select * from truncate_test")
	result.Check(nil)
}

// TestInTxnExecDDLFail tests the following case:
//  1. Execute the SQL of "begin";
//  2. A SQL that will fail to execute;
//  3. Execute DDL.
func TestInTxnExecDDLFail(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t (i int key);")
	tk.MustExec("insert into t values (1);")
	tk.MustExec("begin;")
	tk.MustExec("insert into t values (1);")
	_, err := tk.Exec("truncate table t;")
	require.EqualError(t, err, "[kv:1062]Duplicate entry '1' for key 'PRIMARY'")
	result := tk.MustQuery("select count(*) from t")
	result.Check(testkit.Rows("1"))
}

func TestInTxnExecDDLInvalid(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t;")
	tk.MustExec("create table t (c_int int, c_str varchar(40));")
	tk.MustExec("insert into t values (1, 'quizzical hofstadter');")
	tk.MustExec("begin;")
	_ = tk.MustQuery("select c_int from t where c_str is not null for update;")
	tk.MustExec("alter table t add index idx_4 (c_str);")
}

func TestCreateTable(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	// Test create an exist database
	_, err := tk.Exec("CREATE database test")
	require.Error(t, err)

	// Test create an exist table
	tk.MustExec("CREATE TABLE create_test (id INT NOT NULL DEFAULT 1, name varchar(255), PRIMARY KEY(id));")

	_, err = tk.Exec("CREATE TABLE create_test (id INT NOT NULL DEFAULT 1, name varchar(255), PRIMARY KEY(id));")
	require.Error(t, err)

	// Test "if not exist"
	tk.MustExec("CREATE TABLE if not exists test(id INT NOT NULL DEFAULT 1, name varchar(255), PRIMARY KEY(id));")

	// Testcase for https://github.com/pingcap/tidb/issues/312
	tk.MustExec(`create table issue312_1 (c float(24));`)
	tk.MustExec(`create table issue312_2 (c float(25));`)
	rs, err := tk.Exec(`desc issue312_1`)
	require.NoError(t, err)
	ctx := context.Background()
	req := rs.NewChunk(nil)
	it := chunk.NewIterator4Chunk(req)
	for {
		err1 := rs.Next(ctx, req)
		require.NoError(t, err1)
		if req.NumRows() == 0 {
			break
		}
		for row := it.Begin(); row != it.End(); row = it.Next() {
			require.Equal(t, "float", row.GetString(1))
		}
	}
	rs, err = tk.Exec(`desc issue312_2`)
	require.NoError(t, err)
	req = rs.NewChunk(nil)
	it = chunk.NewIterator4Chunk(req)
	for {
		err1 := rs.Next(ctx, req)
		require.NoError(t, err1)
		if req.NumRows() == 0 {
			break
		}
		for row := it.Begin(); row != it.End(); row = it.Next() {
			require.Equal(t, "double", req.GetRow(0).GetString(1))
		}
	}
	require.NoError(t, rs.Close())

	// test multiple collate specified in column when create.
	tk.MustExec("drop table if exists test_multiple_column_collate;")
	tk.MustExec("create table test_multiple_column_collate (a char(1) collate utf8_bin collate utf8_general_ci) charset utf8mb4 collate utf8mb4_bin")
	tt, err := domain.GetDomain(tk.Session()).InfoSchema().TableByName(model.NewCIStr("test"), model.NewCIStr("test_multiple_column_collate"))
	require.NoError(t, err)
	require.Equal(t, "utf8", tt.Cols()[0].Charset)
	require.Equal(t, "utf8_general_ci", tt.Cols()[0].Collate)
	require.Equal(t, "utf8mb4", tt.Meta().Charset)
	require.Equal(t, "utf8mb4_bin", tt.Meta().Collate)

	tk.MustExec("drop table if exists test_multiple_column_collate;")
	tk.MustExec("create table test_multiple_column_collate (a char(1) charset utf8 collate utf8_bin collate utf8_general_ci) charset utf8mb4 collate utf8mb4_bin")
	tt, err = domain.GetDomain(tk.Session()).InfoSchema().TableByName(model.NewCIStr("test"), model.NewCIStr("test_multiple_column_collate"))
	require.NoError(t, err)
	require.Equal(t, "utf8", tt.Cols()[0].Charset)
	require.Equal(t, "utf8_general_ci", tt.Cols()[0].Collate)
	require.Equal(t, "utf8mb4", tt.Meta().Charset)
	require.Equal(t, "utf8mb4_bin", tt.Meta().Collate)

	// test Err case for multiple collate specified in column when create.
	tk.MustExec("drop table if exists test_err_multiple_collate;")
	_, err = tk.Exec("create table test_err_multiple_collate (a char(1) charset utf8mb4 collate utf8_unicode_ci collate utf8_general_ci) charset utf8mb4 collate utf8mb4_bin")
	require.Error(t, err)
	require.Equal(t, dbterror.ErrCollationCharsetMismatch.GenWithStackByArgs("utf8_unicode_ci", "utf8mb4").Error(), err.Error())

	tk.MustExec("drop table if exists test_err_multiple_collate;")
	_, err = tk.Exec("create table test_err_multiple_collate (a char(1) collate utf8_unicode_ci collate utf8mb4_general_ci) charset utf8mb4 collate utf8mb4_bin")
	require.Error(t, err)
	require.Equal(t, dbterror.ErrCollationCharsetMismatch.GenWithStackByArgs("utf8mb4_general_ci", "utf8").Error(), err.Error())

	// table option is auto-increment
	tk.MustExec("drop table if exists create_auto_increment_test;")
	tk.MustExec("create table create_auto_increment_test (id int not null auto_increment, name varchar(255), primary key(id)) auto_increment = 999;")
	tk.MustExec("insert into create_auto_increment_test (name) values ('aa')")
	tk.MustExec("insert into create_auto_increment_test (name) values ('bb')")
	tk.MustExec("insert into create_auto_increment_test (name) values ('cc')")
	r := tk.MustQuery("select * from create_auto_increment_test;")
	r.Check(testkit.Rows("999 aa", "1000 bb", "1001 cc"))
	tk.MustExec("drop table create_auto_increment_test")
	tk.MustExec("create table create_auto_increment_test (id int not null auto_increment, name varchar(255), primary key(id)) auto_increment = 1999;")
	tk.MustExec("insert into create_auto_increment_test (name) values ('aa')")
	tk.MustExec("insert into create_auto_increment_test (name) values ('bb')")
	tk.MustExec("insert into create_auto_increment_test (name) values ('cc')")
	r = tk.MustQuery("select * from create_auto_increment_test;")
	r.Check(testkit.Rows("1999 aa", "2000 bb", "2001 cc"))
	tk.MustExec("drop table create_auto_increment_test")
	tk.MustExec("create table create_auto_increment_test (id int not null auto_increment, name varchar(255), key(id)) auto_increment = 1000;")
	tk.MustExec("insert into create_auto_increment_test (name) values ('aa')")
	r = tk.MustQuery("select * from create_auto_increment_test;")
	r.Check(testkit.Rows("1000 aa"))

	// Test for `drop table if exists`.
	tk.MustExec("drop table if exists t_if_exists;")
	tk.MustQuery("show warnings;").Check(testkit.Rows("Note 1051 Unknown table 'test.t_if_exists'"))
	tk.MustExec("create table if not exists t1_if_exists(c int)")
	tk.MustExec("drop table if exists t1_if_exists,t2_if_exists,t3_if_exists")
	tk.MustQuery("show warnings").Check(testkit.RowsWithSep("|", "Note|1051|Unknown table 'test.t2_if_exists'", "Note|1051|Unknown table 'test.t3_if_exists'"))
}

func TestCreateView(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	// create an source table
	tk.MustExec("CREATE TABLE source_table (id INT NOT NULL DEFAULT 1, name varchar(255), PRIMARY KEY(id));")
	// test create a exist view
	tk.MustExec("CREATE VIEW view_t AS select id , name from source_table")
	defer tk.MustExec("DROP VIEW IF EXISTS view_t")
	_, err := tk.Exec("CREATE VIEW view_t AS select id , name from source_table")
	require.EqualError(t, err, "[schema:1050]Table 'test.view_t' already exists")
	// create view on nonexistent table
	_, err = tk.Exec("create view v1 (c,d) as select a,b from t1")
	require.EqualError(t, err, "[schema:1146]Table 'test.t1' doesn't exist")
	// simple view
	tk.MustExec("create table t1 (a int ,b int)")
	tk.MustExec("insert into t1 values (1,2), (1,3), (2,4), (2,5), (3,10)")
	// view with colList and SelectFieldExpr
	tk.MustExec("create view v1 (c) as select b+1 from t1")
	// view with SelectFieldExpr
	tk.MustExec("create view v2 as select b+1 from t1")
	// view with SelectFieldExpr and AsName
	tk.MustExec("create view v3 as select b+1 as c from t1")
	// view with colList , SelectField and AsName
	tk.MustExec("create view v4 (c) as select b+1 as d from t1")
	// view with select wild card
	tk.MustExec("create view v5 as select * from t1")
	tk.MustExec("create view v6 (c,d) as select * from t1")
	_, err = tk.Exec("create view v7 (c,d,e) as select * from t1")
	require.Equal(t, dbterror.ErrViewWrongList.Error(), err.Error())
	// drop multiple views in a statement
	tk.MustExec("drop view v1,v2,v3,v4,v5,v6")
	// view with variable
	tk.MustExec("create view v1 (c,d) as select a,b+@@global.max_user_connections from t1")
	_, err = tk.Exec("create view v1 (c,d) as select a,b from t1 where a = @@global.max_user_connections")
	require.EqualError(t, err, "[schema:1050]Table 'test.v1' already exists")
	tk.MustExec("drop view v1")
	// view with different col counts
	_, err = tk.Exec("create view v1 (c,d,e) as select a,b from t1 ")
	require.Equal(t, dbterror.ErrViewWrongList.Error(), err.Error())
	_, err = tk.Exec("create view v1 (c) as select a,b from t1 ")
	require.Equal(t, dbterror.ErrViewWrongList.Error(), err.Error())
	// view with or_replace flag
	tk.MustExec("drop view if exists v1")
	tk.MustExec("create view v1 (c,d) as select a,b from t1")
	tk.MustExec("create or replace view v1 (c,d) as select a,b from t1 ")
	tk.MustExec("create table if not exists t1 (a int ,b int)")
	_, err = tk.Exec("create or replace view t1 as select * from t1")
	require.Equal(t, dbterror.ErrWrongObject.GenWithStackByArgs("test", "t1", "VIEW").Error(), err.Error())
	// create view using prepare
	tk.MustExec(`prepare stmt from "create view v10 (x) as select 1";`)
	tk.MustExec("execute stmt")

	// create view on union
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("drop view if exists v")
	_, err = tk.Exec("create view v as select * from t1 union select * from t2")
	require.True(t, terror.ErrorEqual(err, infoschema.ErrTableNotExists))
	tk.MustExec("create table t1(a int, b int)")
	tk.MustExec("create table t2(a int, b int)")
	tk.MustExec("insert into t1 values(1,2), (1,1), (1,2)")
	tk.MustExec("insert into t2 values(1,1),(1,3)")
	tk.MustExec("create definer='root'@'localhost' view v as select * from t1 union select * from t2")
	tk.MustQuery("select * from v").Sort().Check(testkit.Rows("1 1", "1 2", "1 3"))
	tk.MustExec("alter table t1 drop column a")
	_, err = tk.Exec("select * from v")
	require.True(t, terror.ErrorEqual(err, plannercore.ErrViewInvalid))
	tk.MustExec("alter table t1 add column a int")
	tk.MustQuery("select * from v").Sort().Check(testkit.Rows("1 1", "1 3", "<nil> 1", "<nil> 2"))
	tk.MustExec("alter table t1 drop column a")
	tk.MustExec("alter table t2 drop column b")
	_, err = tk.Exec("select * from v")
	require.True(t, terror.ErrorEqual(err, plannercore.ErrViewInvalid))
	tk.MustExec("drop view v")

	tk.MustExec("create view v as (select * from t1)")
	tk.MustExec("drop view v")
	tk.MustExec("create view v as (select * from t1 union select * from t2)")
	tk.MustExec("drop view v")

	// Test for `drop view if exists`.
	tk.MustExec("drop view if exists v_if_exists;")
	tk.MustQuery("show warnings;").Check(testkit.Rows("Note 1051 Unknown table 'test.v_if_exists'"))
	tk.MustExec("create view v1_if_exists as (select * from t1)")
	tk.MustExec("drop view if exists v1_if_exists,v2_if_exists,v3_if_exists")
	tk.MustQuery("show warnings").Check(testkit.RowsWithSep("|", "Note|1051|Unknown table 'test.v2_if_exists'", "Note|1051|Unknown table 'test.v3_if_exists'"))

	// Test for create nested view.
	tk.MustExec("create table test_v_nested(a int)")
	tk.MustExec("create definer='root'@'localhost' view v_nested as select * from test_v_nested")
	tk.MustExec("create definer='root'@'localhost' view v_nested2 as select * from v_nested")
	_, err = tk.Exec("create or replace definer='root'@'localhost' view v_nested as select * from v_nested2")
	require.True(t, terror.ErrorEqual(err, plannercore.ErrNoSuchTable))
	tk.MustExec("drop table test_v_nested")
	tk.MustExec("drop view v_nested, v_nested2")

	// Refer https://github.com/pingcap/tidb/issues/25876
	err = tk.ExecToErr("create view v_stale as select * from source_table as of timestamp current_timestamp(3)")
	require.Truef(t, terror.ErrorEqual(err, executor.ErrViewInvalid), "err %s", err)
}

func TestViewRecursion(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table if not exists t(a int)")
	tk.MustExec("create definer='root'@'localhost' view recursive_view1 as select * from t")
	tk.MustExec("create definer='root'@'localhost' view recursive_view2 as select * from recursive_view1")
	tk.MustExec("drop table t")
	tk.MustExec("rename table recursive_view2 to t")
	_, err := tk.Exec("select * from recursive_view1")
	require.True(t, terror.ErrorEqual(err, plannercore.ErrViewRecursive))
	tk.MustExec("drop view recursive_view1, t")
}

func TestIssue16250(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table if not exists t(a int)")
	tk.MustExec("create view view_issue16250 as select * from t")
	_, err := tk.Exec("truncate table view_issue16250")
	require.EqualError(t, err, "[schema:1146]Table 'test.view_issue16250' doesn't exist")
}

func TestIssue24771(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec(`drop table if exists zy_tab;`)
	tk.MustExec(`create table if not exists zy_tab (
						zy_code int,
						zy_name varchar(100)
					);`)
	tk.MustExec(`drop table if exists bj_tab;`)
	tk.MustExec(`create table if not exists bj_tab (
						bj_code int,
						bj_name varchar(100),
						bj_addr varchar(100),
						bj_person_count int,
						zy_code int
					);`)
	tk.MustExec(`drop table if exists st_tab;`)
	tk.MustExec(`create table if not exists st_tab (
						st_code int,
						st_name varchar(100),
						bj_code int
					);`)
	tk.MustExec(`drop view if exists v_st_2;`)
	tk.MustExec(`create definer='root'@'localhost' view v_st_2 as
		select st.st_name,bj.bj_name,zy.zy_name
		from (
			select bj_code,
				bj_name,
				zy_code
			from bj_tab as b
			where b.bj_code = 1
		) as bj
		left join zy_tab as zy on zy.zy_code = bj.zy_code
		left join st_tab as st on bj.bj_code = st.bj_code;`)
	tk.MustQuery(`show create view v_st_2`)
	tk.MustQuery(`select * from v_st_2`)
}

func TestTruncateSequence(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create sequence if not exists seq")
	_, err := tk.Exec("truncate table seq")
	require.EqualError(t, err, "[schema:1146]Table 'test.seq' doesn't exist")
	tk.MustExec("create sequence if not exists seq1 start 10 increment 2 maxvalue 10000 cycle")
	_, err = tk.Exec("truncate table seq1")
	require.EqualError(t, err, "[schema:1146]Table 'test.seq1' doesn't exist")
	tk.MustExec("drop sequence if exists seq")
	tk.MustExec("drop sequence if exists seq1")
}

func TestCreateViewWithOverlongColName(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t(a int)")
	defer tk.MustExec("drop table t")
	tk.MustExec("create view v as select distinct'" + strings.Repeat("a", 65) + "', " +
		"max('" + strings.Repeat("b", 65) + "'), " +
		"'cccccccccc', '" + strings.Repeat("d", 65) + "';")
	resultCreateStmt := "CREATE ALGORITHM=UNDEFINED DEFINER=``@`` SQL SECURITY DEFINER VIEW `v` (`name_exp_1`, `name_exp_2`, `cccccccccc`, `name_exp_4`) AS " +
		"SELECT DISTINCT _UTF8MB4'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' AS `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`," +
		"MAX(_UTF8MB4'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb') AS `max('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb')`," +
		"_UTF8MB4'cccccccccc' AS `cccccccccc`,_UTF8MB4'ddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd' AS `ddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd`"
	tk.MustQuery("select * from v")
	tk.MustQuery("select name_exp_1, name_exp_2, cccccccccc, name_exp_4 from v")
	tk.MustQuery("show create view v").Check(testkit.Rows("v " + resultCreateStmt + " utf8mb4 utf8mb4_bin"))
	tk.MustExec("drop view v;")
	tk.MustExec(resultCreateStmt)

	tk.MustExec("drop view v ")
	tk.MustExec("create definer='root'@'localhost' view v as select 'a', '" + strings.Repeat("b", 65) + "' from t " +
		"union select '" + strings.Repeat("c", 65) + "', " +
		"count(distinct '" + strings.Repeat("b", 65) + "', " +
		"'c');")
	resultCreateStmt = "CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`localhost` SQL SECURITY DEFINER VIEW `v` (`a`, `name_exp_2`) AS " +
		"SELECT _UTF8MB4'a' AS `a`,_UTF8MB4'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' AS `bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb` FROM `test`.`t` " +
		"UNION SELECT _UTF8MB4'ccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc' AS `ccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc`," +
		"COUNT(DISTINCT _UTF8MB4'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', _UTF8MB4'c') AS `count(distinct 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 'c')`"
	tk.MustQuery("select * from v")
	tk.MustQuery("select a, name_exp_2 from v")
	tk.MustQuery("show create view v").Check(testkit.Rows("v " + resultCreateStmt + " utf8mb4 utf8mb4_bin"))
	tk.MustExec("drop view v;")
	tk.MustExec(resultCreateStmt)

	tk.MustExec("drop view v ")
	tk.MustExec("create definer='root'@'localhost' view v as select 'a' as '" + strings.Repeat("b", 65) + "' from t;")
	tk.MustQuery("select * from v")
	tk.MustQuery("select name_exp_1 from v")
	resultCreateStmt = "CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`localhost` SQL SECURITY DEFINER VIEW `v` (`name_exp_1`) AS SELECT _UTF8MB4'a' AS `" + strings.Repeat("b", 65) + "` FROM `test`.`t`"
	tk.MustQuery("show create view v").Check(testkit.Rows("v " + resultCreateStmt + " utf8mb4 utf8mb4_bin"))
	tk.MustExec("drop view v;")
	tk.MustExec(resultCreateStmt)

	tk.MustExec("drop view v ")
	err := tk.ExecToErr("create view v(`" + strings.Repeat("b", 65) + "`) as select a from t;")
	require.EqualError(t, err, "[ddl:1059]Identifier name 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' is too long")
}

func TestCreateDropDatabase(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("create database if not exists drop_test;")
	tk.MustExec("drop database if exists drop_test;")
	tk.MustExec("create database drop_test;")
	tk.MustExec("use drop_test;")
	tk.MustExec("drop database drop_test;")
	_, err := tk.Exec("drop table t;")
	require.Equal(t, plannercore.ErrNoDB.Error(), err.Error())
	err = tk.ExecToErr("select * from t;")
	require.Equal(t, plannercore.ErrNoDB.Error(), err.Error())

	_, err = tk.Exec("drop database mysql")
	require.Error(t, err)

	tk.MustExec("create database charset_test charset ascii;")
	tk.MustQuery("show create database charset_test;").Check(testkit.RowsWithSep("|",
		"charset_test|CREATE DATABASE `charset_test` /*!40100 DEFAULT CHARACTER SET ascii */",
	))
	tk.MustExec("drop database charset_test;")
	tk.MustExec("create database charset_test charset binary;")
	tk.MustQuery("show create database charset_test;").Check(testkit.RowsWithSep("|",
		"charset_test|CREATE DATABASE `charset_test` /*!40100 DEFAULT CHARACTER SET binary */",
	))
	tk.MustExec("drop database charset_test;")
	tk.MustExec("create database charset_test collate utf8_general_ci;")
	tk.MustQuery("show create database charset_test;").Check(testkit.RowsWithSep("|",
		"charset_test|CREATE DATABASE `charset_test` /*!40100 DEFAULT CHARACTER SET utf8 COLLATE utf8_general_ci */",
	))
	tk.MustExec("drop database charset_test;")
	tk.MustExec("create database charset_test charset utf8 collate utf8_general_ci;")
	tk.MustQuery("show create database charset_test;").Check(testkit.RowsWithSep("|",
		"charset_test|CREATE DATABASE `charset_test` /*!40100 DEFAULT CHARACTER SET utf8 COLLATE utf8_general_ci */",
	))
	tk.MustGetErrMsg("create database charset_test charset utf8 collate utf8mb4_unicode_ci;", "[ddl:1253]COLLATION 'utf8mb4_unicode_ci' is not valid for CHARACTER SET 'utf8'")

	tk.MustExec("SET SESSION character_set_server='ascii'")
	tk.MustExec("SET SESSION collation_server='ascii_bin'")

	tk.MustExec("drop database charset_test;")
	tk.MustExec("create database charset_test;")
	tk.MustQuery("show create database charset_test;").Check(testkit.RowsWithSep("|",
		"charset_test|CREATE DATABASE `charset_test` /*!40100 DEFAULT CHARACTER SET ascii */",
	))

	tk.MustExec("drop database charset_test;")
	tk.MustExec("create database charset_test collate utf8mb4_general_ci;")
	tk.MustQuery("show create database charset_test;").Check(testkit.RowsWithSep("|",
		"charset_test|CREATE DATABASE `charset_test` /*!40100 DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci */",
	))

	tk.MustExec("drop database charset_test;")
	tk.MustExec("create database charset_test charset utf8mb4;")
	tk.MustQuery("show create database charset_test;").Check(testkit.RowsWithSep("|",
		"charset_test|CREATE DATABASE `charset_test` /*!40100 DEFAULT CHARACTER SET utf8mb4 */",
	))
}

func TestCreateDropTable(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table if not exists drop_test (a int)")
	tk.MustExec("drop table if exists drop_test")
	tk.MustExec("create table drop_test (a int)")
	tk.MustExec("drop table drop_test")

	_, err := tk.Exec("drop table mysql.gc_delete_range")
	require.Error(t, err)
}

func TestCreateDropView(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create or replace view drop_test as select 1,2")

	_, err := tk.Exec("drop table drop_test")
	require.EqualError(t, err, "[schema:1051]Unknown table 'test.drop_test'")

	tk.MustExec("drop view if exists drop_test")

	_, err = tk.Exec("drop view mysql.gc_delete_range")
	require.EqualError(t, err, "Drop tidb system table 'mysql.gc_delete_range' is forbidden")

	_, err = tk.Exec("drop view drop_test")
	require.EqualError(t, err, "[schema:1051]Unknown table 'test.drop_test'")

	tk.MustExec("create table t_v(a int)")
	_, err = tk.Exec("drop view t_v")
	require.EqualError(t, err, "[ddl:1347]'test.t_v' is not VIEW")

	tk.MustExec("create table t_v1(a int, b int);")
	tk.MustExec("create table t_v2(a int, b int);")
	tk.MustExec("create view v as select * from t_v1;")
	tk.MustExec("create or replace view v  as select * from t_v2;")
	tk.MustQuery("select * from information_schema.views where table_name ='v';").Check(
		testkit.Rows("def test v SELECT `test`.`t_v2`.`a` AS `a`,`test`.`t_v2`.`b` AS `b` FROM `test`.`t_v2` CASCADED NO @ DEFINER utf8mb4 utf8mb4_bin"))
}

func TestCreateDropIndex(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table if not exists drop_test (a int)")
	tk.MustExec("create index idx_a on drop_test (a)")
	tk.MustExec("drop index idx_a on drop_test")
	tk.MustExec("drop table drop_test")
}

func TestAlterTableAddColumn(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table if not exists alter_test (c1 int)")
	tk.MustExec("insert into alter_test values(1)")
	tk.MustExec("alter table alter_test add column c2 timestamp default current_timestamp")
	time.Sleep(1 * time.Millisecond)
	now := time.Now().Add(-1 * time.Millisecond).Format(types.TimeFormat)
	r, err := tk.Exec("select c2 from alter_test")
	require.NoError(t, err)
	req := r.NewChunk(nil)
	err = r.Next(context.Background(), req)
	require.NoError(t, err)
	row := req.GetRow(0)
	require.Equal(t, 1, row.Len())
	require.GreaterOrEqual(t, now, row.GetTime(0).String())
	require.Nil(t, r.Close())
	tk.MustExec("alter table alter_test add column c3 varchar(50) default 'CURRENT_TIMESTAMP'")
	tk.MustQuery("select c3 from alter_test").Check(testkit.Rows("CURRENT_TIMESTAMP"))
	tk.MustExec("create or replace view alter_view as select c1,c2 from alter_test")
	_, err = tk.Exec("alter table alter_view add column c4 varchar(50)")
	require.Equal(t, dbterror.ErrWrongObject.GenWithStackByArgs("test", "alter_view", "BASE TABLE").Error(), err.Error())
	tk.MustExec("drop view alter_view")
	tk.MustExec("create sequence alter_seq")
	_, err = tk.Exec("alter table alter_seq add column c int")
	require.Equal(t, dbterror.ErrWrongObject.GenWithStackByArgs("test", "alter_seq", "BASE TABLE").Error(), err.Error())
	tk.MustExec("drop sequence alter_seq")
}

func TestAlterTableAddColumns(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table if not exists alter_test (c1 int)")
	tk.MustExec("insert into alter_test values(1)")
	tk.MustExec("alter table alter_test add column c2 timestamp default current_timestamp, add column c8 varchar(50) default 'CURRENT_TIMESTAMP'")
	tk.MustExec("alter table alter_test add column (c7 timestamp default current_timestamp, c3 varchar(50) default 'CURRENT_TIMESTAMP')")
	r, err := tk.Exec("select c2 from alter_test")
	require.NoError(t, err)
	req := r.NewChunk(nil)
	err = r.Next(context.Background(), req)
	require.NoError(t, err)
	row := req.GetRow(0)
	require.Equal(t, 1, row.Len())
	require.Nil(t, r.Close())
	tk.MustQuery("select c3 from alter_test").Check(testkit.Rows("CURRENT_TIMESTAMP"))
	tk.MustExec("create or replace view alter_view as select c1,c2 from alter_test")
	_, err = tk.Exec("alter table alter_view add column (c4 varchar(50), c5 varchar(50))")
	require.Equal(t, dbterror.ErrWrongObject.GenWithStackByArgs("test", "alter_view", "BASE TABLE").Error(), err.Error())
	tk.MustExec("drop view alter_view")
	tk.MustExec("create sequence alter_seq")
	_, err = tk.Exec("alter table alter_seq add column (c1 int, c2 varchar(10))")
	require.Equal(t, dbterror.ErrWrongObject.GenWithStackByArgs("test", "alter_seq", "BASE TABLE").Error(), err.Error())
	tk.MustExec("drop sequence alter_seq")
}

func TestAddNotNullColumnNoDefault(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table nn (c1 int)")
	tk.MustExec("insert nn values (1), (2)")
	tk.MustExec("alter table nn add column c2 int not null")

	tbl, err := domain.GetDomain(tk.Session()).InfoSchema().TableByName(model.NewCIStr("test"), model.NewCIStr("nn"))
	require.NoError(t, err)
	col2 := tbl.Meta().Columns[1]
	require.Nil(t, col2.DefaultValue)
	require.Equal(t, "0", col2.OriginDefaultValue)

	tk.MustQuery("select * from nn").Check(testkit.Rows("1 0", "2 0"))
	_, err = tk.Exec("insert nn (c1) values (3)")
	require.Error(t, err)
	tk.MustExec("set sql_mode=''")
	tk.MustExec("insert nn (c1) values (3)")
	tk.MustQuery("select * from nn").Check(testkit.Rows("1 0", "2 0", "3 0"))
}

func TestAlterTableModifyColumn(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists mc")
	tk.MustExec("create table mc(c1 int, c2 varchar(10), c3 bit)")
	_, err := tk.Exec("alter table mc modify column c1 short")
	require.Error(t, err)
	tk.MustExec("alter table mc modify column c1 bigint")

	_, err = tk.Exec("alter table mc modify column c2 blob")
	require.Error(t, err)

	_, err = tk.Exec("alter table mc modify column c2 varchar(8)")
	require.NoError(t, err)
	tk.MustExec("alter table mc modify column c2 varchar(11)")
	tk.MustExec("alter table mc modify column c2 text(13)")
	tk.MustExec("alter table mc modify column c2 text")
	tk.MustExec("alter table mc modify column c3 bit")
	result := tk.MustQuery("show create table mc")
	createSQL := result.Rows()[0][1]
	expected := "CREATE TABLE `mc` (\n  `c1` bigint(20) DEFAULT NULL,\n  `c2` text DEFAULT NULL,\n  `c3` bit(1) DEFAULT NULL\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"
	require.Equal(t, expected, createSQL)
	tk.MustExec("create or replace view alter_view as select c1,c2 from mc")
	_, err = tk.Exec("alter table alter_view modify column c2 text")
	require.Equal(t, dbterror.ErrWrongObject.GenWithStackByArgs("test", "alter_view", "BASE TABLE").Error(), err.Error())
	tk.MustExec("drop view alter_view")
	tk.MustExec("create sequence alter_seq")
	_, err = tk.Exec("alter table alter_seq modify column c int")
	require.Equal(t, dbterror.ErrWrongObject.GenWithStackByArgs("test", "alter_seq", "BASE TABLE").Error(), err.Error())
	tk.MustExec("drop sequence alter_seq")

	// test multiple collate modification in column.
	tk.MustExec("drop table if exists modify_column_multiple_collate")
	tk.MustExec("create table modify_column_multiple_collate (a char(1) collate utf8_bin collate utf8_general_ci) charset utf8mb4 collate utf8mb4_bin")
	_, err = tk.Exec("alter table modify_column_multiple_collate modify column a char(1) collate utf8mb4_bin;")
	require.NoError(t, err)
	tt, err := domain.GetDomain(tk.Session()).InfoSchema().TableByName(model.NewCIStr("test"), model.NewCIStr("modify_column_multiple_collate"))
	require.NoError(t, err)
	require.Equal(t, "utf8mb4", tt.Cols()[0].Charset)
	require.Equal(t, "utf8mb4_bin", tt.Cols()[0].Collate)
	require.Equal(t, "utf8mb4", tt.Meta().Charset)
	require.Equal(t, "utf8mb4_bin", tt.Meta().Collate)

	tk.MustExec("drop table if exists modify_column_multiple_collate;")
	tk.MustExec("create table modify_column_multiple_collate (a char(1) collate utf8_bin collate utf8_general_ci) charset utf8mb4 collate utf8mb4_bin")
	_, err = tk.Exec("alter table modify_column_multiple_collate modify column a char(1) charset utf8mb4 collate utf8mb4_bin;")
	require.NoError(t, err)
	tt, err = domain.GetDomain(tk.Session()).InfoSchema().TableByName(model.NewCIStr("test"), model.NewCIStr("modify_column_multiple_collate"))
	require.NoError(t, err)
	require.Equal(t, "utf8mb4", tt.Cols()[0].Charset)
	require.Equal(t, "utf8mb4_bin", tt.Cols()[0].Collate)
	require.Equal(t, "utf8mb4", tt.Meta().Charset)
	require.Equal(t, "utf8mb4_bin", tt.Meta().Collate)

	// test Err case for multiple collate modification in column.
	tk.MustExec("drop table if exists err_modify_multiple_collate;")
	tk.MustExec("create table err_modify_multiple_collate (a char(1) collate utf8_bin collate utf8_general_ci) charset utf8mb4 collate utf8mb4_bin")
	_, err = tk.Exec("alter table err_modify_multiple_collate modify column a char(1) charset utf8mb4 collate utf8_bin;")
	require.Error(t, err)
	require.Equal(t, dbterror.ErrCollationCharsetMismatch.GenWithStackByArgs("utf8_bin", "utf8mb4").Error(), err.Error())

	tk.MustExec("drop table if exists err_modify_multiple_collate;")
	tk.MustExec("create table err_modify_multiple_collate (a char(1) collate utf8_bin collate utf8_general_ci) charset utf8mb4 collate utf8mb4_bin")
	_, err = tk.Exec("alter table err_modify_multiple_collate modify column a char(1) collate utf8_bin collate utf8mb4_bin;")
	require.Error(t, err)
	require.Equal(t, dbterror.ErrCollationCharsetMismatch.GenWithStackByArgs("utf8mb4_bin", "utf8").Error(), err.Error())

}

func TestColumnCharsetAndCollate(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	dbName := "col_charset_collate"
	tk.MustExec("create database " + dbName)
	tk.MustExec("use " + dbName)
	tests := []struct {
		colType     string
		charset     string
		collates    string
		exptCharset string
		exptCollate string
		errMsg      string
	}{
		{
			colType:     "varchar(10)",
			charset:     "charset utf8",
			collates:    "collate utf8_bin",
			exptCharset: "utf8",
			exptCollate: "utf8_bin",
			errMsg:      "",
		},
		{
			colType:     "varchar(10)",
			charset:     "charset utf8mb4",
			collates:    "",
			exptCharset: "utf8mb4",
			exptCollate: "utf8mb4_bin",
			errMsg:      "",
		},
		{
			colType:     "varchar(10)",
			charset:     "charset utf16",
			collates:    "",
			exptCharset: "",
			exptCollate: "",
			errMsg:      "Unknown charset utf16",
		},
		{
			colType:     "varchar(10)",
			charset:     "charset latin1",
			collates:    "",
			exptCharset: "latin1",
			exptCollate: "latin1_bin",
			errMsg:      "",
		},
		{
			colType:     "varchar(10)",
			charset:     "charset binary",
			collates:    "",
			exptCharset: "binary",
			exptCollate: "binary",
			errMsg:      "",
		},
		{
			colType:     "varchar(10)",
			charset:     "charset ascii",
			collates:    "",
			exptCharset: "ascii",
			exptCollate: "ascii_bin",
			errMsg:      "",
		},
	}
	sctx := tk.Session()
	dm := domain.GetDomain(sctx)
	for i, tt := range tests {
		tblName := fmt.Sprintf("t%d", i)
		sql := fmt.Sprintf("create table %s (a %s %s %s)", tblName, tt.colType, tt.charset, tt.collates)
		if tt.errMsg == "" {
			tk.MustExec(sql)
			is := dm.InfoSchema()
			require.NotNil(t, is)

			tb, err := is.TableByName(model.NewCIStr(dbName), model.NewCIStr(tblName))
			require.NoError(t, err)
			require.Equalf(t, tt.exptCharset, tb.Meta().Columns[0].Charset, sql)
			require.Equalf(t, tt.exptCollate, tb.Meta().Columns[0].Collate, sql)
		} else {
			_, err := tk.Exec(sql)
			require.Errorf(t, err, sql)
		}
	}
	tk.MustExec("drop database " + dbName)
}

func TestTooLargeIdentifierLength(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)

	// for database.
	dbName1, dbName2 := strings.Repeat("a", mysql.MaxDatabaseNameLength), strings.Repeat("a", mysql.MaxDatabaseNameLength+1)
	tk.MustExec(fmt.Sprintf("create database %s", dbName1))
	tk.MustExec(fmt.Sprintf("drop database %s", dbName1))
	_, err := tk.Exec(fmt.Sprintf("create database %s", dbName2))
	require.Equal(t, fmt.Sprintf("[ddl:1059]Identifier name '%s' is too long", dbName2), err.Error())

	// for table.
	tk.MustExec("use test")
	tableName1, tableName2 := strings.Repeat("b", mysql.MaxTableNameLength), strings.Repeat("b", mysql.MaxTableNameLength+1)
	tk.MustExec(fmt.Sprintf("create table %s(c int)", tableName1))
	tk.MustExec(fmt.Sprintf("drop table %s", tableName1))
	_, err = tk.Exec(fmt.Sprintf("create table %s(c int)", tableName2))
	require.Equal(t, fmt.Sprintf("[ddl:1059]Identifier name '%s' is too long", tableName2), err.Error())

	// for column.
	tk.MustExec("drop table if exists t;")
	columnName1, columnName2 := strings.Repeat("c", mysql.MaxColumnNameLength), strings.Repeat("c", mysql.MaxColumnNameLength+1)
	tk.MustExec(fmt.Sprintf("create table t(%s int)", columnName1))
	tk.MustExec("drop table t")
	_, err = tk.Exec(fmt.Sprintf("create table t(%s int)", columnName2))
	require.Equal(t, fmt.Sprintf("[ddl:1059]Identifier name '%s' is too long", columnName2), err.Error())

	// for index.
	tk.MustExec("create table t(c int);")
	indexName1, indexName2 := strings.Repeat("d", mysql.MaxIndexIdentifierLen), strings.Repeat("d", mysql.MaxIndexIdentifierLen+1)
	tk.MustExec(fmt.Sprintf("create index %s on t(c)", indexName1))
	tk.MustExec(fmt.Sprintf("drop index %s on t", indexName1))
	_, err = tk.Exec(fmt.Sprintf("create index %s on t(c)", indexName2))
	require.Equal(t, fmt.Sprintf("[ddl:1059]Identifier name '%s' is too long", indexName2), err.Error())

	// for create table with index.
	tk.MustExec("drop table t;")
	_, err = tk.Exec(fmt.Sprintf("create table t(c int, index %s(c));", indexName2))
	require.Equal(t, fmt.Sprintf("[ddl:1059]Identifier name '%s' is too long", indexName2), err.Error())
}

func TestShardRowIDBits(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)

	tk.MustExec("use test")
	tk.MustExec("create table t (a int) shard_row_id_bits = 15")
	for i := 0; i < 100; i++ {
		tk.MustExec("insert into t values (?)", i)
	}

	dom := domain.GetDomain(tk.Session())
	tbl, err := dom.InfoSchema().TableByName(model.NewCIStr("test"), model.NewCIStr("t"))
	require.NoError(t, err)

	assertCountAndShard := func(tt table.Table, expectCount int) {
		var hasShardedID bool
		var count int
		require.NoError(t, tk.Session().NewTxn(context.Background()))
		err = tables.IterRecords(tt, tk.Session(), nil, func(h kv.Handle, rec []types.Datum, cols []*table.Column) (more bool, err error) {
			require.GreaterOrEqual(t, h.IntValue(), int64(0))
			first8bits := h.IntValue() >> 56
			if first8bits > 0 {
				hasShardedID = true
			}
			count++
			return true, nil
		})
		require.NoError(t, err)
		require.Equal(t, expectCount, count)
		require.True(t, hasShardedID)
	}

	assertCountAndShard(tbl, 100)

	// After PR 10759, shard_row_id_bits is supported with tables with auto_increment column.
	tk.MustExec("create table auto (id int not null auto_increment unique) shard_row_id_bits = 4")
	tk.MustExec("alter table auto shard_row_id_bits = 5")
	tk.MustExec("drop table auto")
	tk.MustExec("create table auto (id int not null auto_increment unique) shard_row_id_bits = 0")
	tk.MustExec("alter table auto shard_row_id_bits = 5")
	tk.MustExec("drop table auto")
	tk.MustExec("create table auto (id int not null auto_increment unique)")
	tk.MustExec("alter table auto shard_row_id_bits = 5")
	tk.MustExec("drop table auto")
	tk.MustExec("create table auto (id int not null auto_increment unique) shard_row_id_bits = 4")
	tk.MustExec("alter table auto shard_row_id_bits = 0")
	tk.MustExec("drop table auto")

	errMsg := "[ddl:8200]Unsupported shard_row_id_bits for table with primary key as row id"
	tk.MustGetErrMsg("create table auto (id varchar(255) primary key clustered, b int) shard_row_id_bits = 4;", errMsg)
	tk.MustExec("create table auto (id varchar(255) primary key clustered, b int) shard_row_id_bits = 0;")
	tk.MustGetErrMsg("alter table auto shard_row_id_bits = 5;", errMsg)
	tk.MustExec("alter table auto shard_row_id_bits = 0;")
	tk.MustExec("drop table if exists auto;")

	// After PR 10759, shard_row_id_bits is not supported with pk_is_handle tables.
	tk.MustGetErrMsg("create table auto (id int not null auto_increment primary key, b int) shard_row_id_bits = 4", errMsg)
	tk.MustExec("create table auto (id int not null auto_increment primary key, b int) shard_row_id_bits = 0")
	tk.MustGetErrMsg("alter table auto shard_row_id_bits = 5", errMsg)
	tk.MustExec("alter table auto shard_row_id_bits = 0")

	// Hack an existing table with shard_row_id_bits and primary key as handle
	db, ok := dom.InfoSchema().SchemaByName(model.NewCIStr("test"))
	require.True(t, ok)
	tbl, err = dom.InfoSchema().TableByName(model.NewCIStr("test"), model.NewCIStr("auto"))
	tblInfo := tbl.Meta()
	tblInfo.ShardRowIDBits = 5
	tblInfo.MaxShardRowIDBits = 5

	err = kv.RunInNewTxn(context.Background(), store, false, func(ctx context.Context, txn kv.Transaction) error {
		m := meta.NewMeta(txn)
		_, err = m.GenSchemaVersion()
		require.NoError(t, err)
		require.Nil(t, m.UpdateTable(db.ID, tblInfo))
		return nil
	})
	require.NoError(t, err)
	err = dom.Reload()
	require.NoError(t, err)

	tk.MustExec("insert auto(b) values (1), (3), (5)")
	tk.MustQuery("select id from auto order by id").Check(testkit.Rows("1", "2", "3"))

	tk.MustExec("alter table auto shard_row_id_bits = 0")
	tk.MustExec("drop table auto")

	// Test shard_row_id_bits with auto_increment column
	tk.MustExec("create table auto (a int, b int auto_increment unique) shard_row_id_bits = 15")
	for i := 0; i < 100; i++ {
		tk.MustExec("insert into auto(a) values (?)", i)
	}
	tbl, err = dom.InfoSchema().TableByName(model.NewCIStr("test"), model.NewCIStr("auto"))
	assertCountAndShard(tbl, 100)
	prevB, err := strconv.Atoi(tk.MustQuery("select b from auto where a=0").Rows()[0][0].(string))
	require.NoError(t, err)
	for i := 1; i < 100; i++ {
		b, err := strconv.Atoi(tk.MustQuery(fmt.Sprintf("select b from auto where a=%d", i)).Rows()[0][0].(string))
		require.NoError(t, err)
		require.Greater(t, b, prevB)
		prevB = b
	}

	// Test overflow
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t1 (a int) shard_row_id_bits = 15")
	defer tk.MustExec("drop table if exists t1")

	tbl, err = dom.InfoSchema().TableByName(model.NewCIStr("test"), model.NewCIStr("t1"))
	require.NoError(t, err)
	maxID := 1<<(64-15-1) - 1
	alloc := tbl.Allocators(tk.Session()).Get(autoid.RowIDAllocType)
	err = alloc.Rebase(context.Background(), int64(maxID)-1, false)
	require.NoError(t, err)
	tk.MustExec("insert into t1 values(1)")

	// continue inserting will fail.
	_, err = tk.Exec("insert into t1 values(2)")
	require.Truef(t, autoid.ErrAutoincReadFailed.Equal(err), "err:%v", err)
	_, err = tk.Exec("insert into t1 values(3)")
	require.Truef(t, autoid.ErrAutoincReadFailed.Equal(err), "err:%v", err)
}

func TestAutoRandomBitsData(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)

	tk.MustExec("create database if not exists test_auto_random_bits")
	defer tk.MustExec("drop database if exists test_auto_random_bits")
	tk.MustExec("use test_auto_random_bits")
	tk.MustExec("drop table if exists t")

	extractAllHandles := func() []int64 {
		allHds, err := ddltestutil.ExtractAllTableHandles(tk.Session(), "test_auto_random_bits", "t")
		require.NoError(t, err)
		return allHds
	}

	tk.MustExec("set @@allow_auto_random_explicit_insert = true")

	tk.MustExec("create table t (a bigint primary key clustered auto_random(15), b int)")
	for i := 0; i < 100; i++ {
		tk.MustExec("insert into t(b) values (?)", i)
	}
	allHandles := extractAllHandles()
	tk.MustExec("drop table t")

	// Test auto random id number.
	require.Equal(t, 100, len(allHandles))
	// Test the handles are not all zero.
	allZero := true
	for _, h := range allHandles {
		allZero = allZero && (h>>(64-16)) == 0
	}
	require.False(t, allZero)
	// Test non-shard-bits part of auto random id is monotonic increasing and continuous.
	orderedHandles := testutil.MaskSortHandles(allHandles, 15, mysql.TypeLonglong)
	size := int64(len(allHandles))
	for i := int64(1); i <= size; i++ {
		require.Equal(t, orderedHandles[i-1], i)
	}

	// Test explicit insert.
	autoRandBitsUpperBound := 2<<47 - 1
	tk.MustExec("create table t (a bigint primary key clustered auto_random(15), b int)")
	for i := -10; i < 10; i++ {
		tk.MustExec(fmt.Sprintf("insert into t values(%d, %d)", i+autoRandBitsUpperBound, i))
	}
	_, err := tk.Exec("insert into t (b) values (0)")
	require.Error(t, err)
	require.Equal(t, autoid.ErrAutoRandReadFailed.GenWithStackByArgs().Error(), err.Error())
	tk.MustExec("drop table t")

	// Test overflow.
	tk.MustExec("create table t (a bigint primary key auto_random(15), b int)")
	// Here we cannot fill the all values for a `bigint` column,
	// so firstly we rebase auto_rand to the position before overflow.
	tk.MustExec(fmt.Sprintf("insert into t values (%d, %d)", autoRandBitsUpperBound, 1))
	_, err = tk.Exec("insert into t (b) values (0)")
	require.Error(t, err)
	require.Equal(t, autoid.ErrAutoRandReadFailed.GenWithStackByArgs().Error(), err.Error())
	tk.MustExec("drop table t")

	tk.MustExec("create table t (a bigint primary key auto_random(15), b int)")
	tk.MustExec("insert into t values (1, 2)")
	tk.MustExec(fmt.Sprintf("update t set a = %d where a = 1", autoRandBitsUpperBound))
	_, err = tk.Exec("insert into t (b) values (0)")
	require.Error(t, err)
	require.Equal(t, autoid.ErrAutoRandReadFailed.GenWithStackByArgs().Error(), err.Error())
	tk.MustExec("drop table t")

	// Test insert negative integers explicitly won't trigger rebase.
	tk.MustExec("create table t (a bigint primary key auto_random(15), b int)")
	for i := 1; i <= 100; i++ {
		tk.MustExec("insert into t(b) values (?)", i)
		tk.MustExec("insert into t(a, b) values (?, ?)", -i, i)
	}
	// orderedHandles should be [-100, -99, ..., -2, -1, 1, 2, ..., 99, 100]
	orderedHandles = testutil.MaskSortHandles(extractAllHandles(), 15, mysql.TypeLonglong)
	size = int64(len(allHandles))
	for i := int64(0); i < 100; i++ {
		require.Equal(t, i-100, orderedHandles[i])
	}
	for i := int64(100); i < size; i++ {
		require.Equal(t, i-99, orderedHandles[i])
	}
	tk.MustExec("drop table t")

	// Test signed/unsigned types.
	tk.MustExec("create table t (a bigint primary key auto_random(10), b int)")
	for i := 0; i < 100; i++ {
		tk.MustExec("insert into t (b) values(?)", i)
	}
	for _, h := range extractAllHandles() {
		// Sign bit should be reserved.
		require.True(t, h > 0)
	}
	tk.MustExec("drop table t")

	tk.MustExec("create table t (a bigint unsigned primary key auto_random(10), b int)")
	for i := 0; i < 100; i++ {
		tk.MustExec("insert into t (b) values(?)", i)
	}
	signBitUnused := true
	for _, h := range extractAllHandles() {
		signBitUnused = signBitUnused && (h > 0)
	}
	// Sign bit should be used for shard.
	require.False(t, signBitUnused)
	tk.MustExec("drop table t;")

	// Test rename table does not affect incremental part of auto_random ID.
	tk.MustExec("create database test_auto_random_bits_rename;")
	tk.MustExec("create table t (a bigint auto_random primary key);")
	for i := 0; i < 10; i++ {
		tk.MustExec("insert into t values ();")
	}
	tk.MustExec("alter table t rename to test_auto_random_bits_rename.t1;")
	for i := 0; i < 10; i++ {
		tk.MustExec("insert into test_auto_random_bits_rename.t1 values ();")
	}
	tk.MustExec("alter table test_auto_random_bits_rename.t1 rename to t;")
	for i := 0; i < 10; i++ {
		tk.MustExec("insert into t values ();")
	}
	uniqueHandles := make(map[int64]struct{})
	for _, h := range extractAllHandles() {
		uniqueHandles[h&((1<<(63-5))-1)] = struct{}{}
	}
	require.Equal(t, 30, len(uniqueHandles))
	tk.MustExec("drop database test_auto_random_bits_rename;")
	tk.MustExec("drop table t;")
}

func TestAutoRandomTableOption(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	// test table option is auto-random
	tk.MustExec("drop table if exists auto_random_table_option")
	tk.MustExec("create table auto_random_table_option (a bigint auto_random(5) key) auto_random_base = 1000")
	tt, err := domain.GetDomain(tk.Session()).InfoSchema().TableByName(model.NewCIStr("test"), model.NewCIStr("auto_random_table_option"))
	require.NoError(t, err)
	require.Equal(t, int64(1000), tt.Meta().AutoRandID)
	tk.MustExec("insert into auto_random_table_option values (),(),(),(),()")
	allHandles, err := ddltestutil.ExtractAllTableHandles(tk.Session(), "test", "auto_random_table_option")
	require.NoError(t, err)
	require.Equal(t, 5, len(allHandles))
	// Test non-shard-bits part of auto random id is monotonic increasing and continuous.
	orderedHandles := testutil.MaskSortHandles(allHandles, 5, mysql.TypeLonglong)
	size := int64(len(allHandles))
	for i := int64(0); i < size; i++ {
		require.Equal(t, orderedHandles[i], i+1000)
	}

	tk.MustExec("drop table if exists alter_table_auto_random_option")
	tk.MustExec("create table alter_table_auto_random_option (a bigint primary key auto_random(4), b int)")
	tt, err = domain.GetDomain(tk.Session()).InfoSchema().TableByName(model.NewCIStr("test"), model.NewCIStr("alter_table_auto_random_option"))
	require.NoError(t, err)
	require.Equal(t, int64(0), tt.Meta().AutoRandID)
	tk.MustExec("insert into alter_table_auto_random_option values(),(),(),(),()")
	allHandles, err = ddltestutil.ExtractAllTableHandles(tk.Session(), "test", "alter_table_auto_random_option")
	require.NoError(t, err)
	orderedHandles = testutil.MaskSortHandles(allHandles, 5, mysql.TypeLonglong)
	size = int64(len(allHandles))
	for i := int64(0); i < size; i++ {
		require.Equal(t, i+1, orderedHandles[i])
	}
	tk.MustExec("delete from alter_table_auto_random_option")

	// alter table to change the auto_random option (it will dismiss the local allocator cache)
	// To avoid the new base is in the range of local cache, which will leading the next
	// value is not what we rebased, because the local cache is dropped, here we choose
	// a quite big value to do this.
	tk.MustExec("alter table alter_table_auto_random_option auto_random_base = 3000000")
	tt, err = domain.GetDomain(tk.Session()).InfoSchema().TableByName(model.NewCIStr("test"), model.NewCIStr("alter_table_auto_random_option"))
	require.NoError(t, err)
	require.Equal(t, int64(3000000), tt.Meta().AutoRandID)
	tk.MustExec("insert into alter_table_auto_random_option values(),(),(),(),()")
	allHandles, err = ddltestutil.ExtractAllTableHandles(tk.Session(), "test", "alter_table_auto_random_option")
	require.NoError(t, err)
	orderedHandles = testutil.MaskSortHandles(allHandles, 5, mysql.TypeLonglong)
	size = int64(len(allHandles))
	for i := int64(0); i < size; i++ {
		require.Equal(t, i+3000000, orderedHandles[i])
	}
	tk.MustExec("drop table alter_table_auto_random_option")

	// Alter auto_random_base on non auto_random table.
	tk.MustExec("create table alter_auto_random_normal (a int)")
	_, err = tk.Exec("alter table alter_auto_random_normal auto_random_base = 100")
	require.Error(t, err)
	require.Contains(t, err.Error(), autoid.AutoRandomRebaseNotApplicable)
}

// Test filter different kind of allocators.
// In special ddl type, for example:
// 1: ActionRenameTable             : it will abandon all the old allocators.
// 2: ActionRebaseAutoID            : it will drop row-id-type allocator.
// 3: ActionModifyTableAutoIdCache  : it will drop row-id-type allocator.
// 3: ActionRebaseAutoRandomBase    : it will drop auto-rand-type allocator.
func TestFilterDifferentAllocators(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("drop table if exists t1")

	tk.MustExec("create table t(a bigint auto_random(5) key, b int auto_increment unique)")
	tk.MustExec("insert into t values()")
	tk.MustQuery("select b from t").Check(testkit.Rows("1"))
	allHandles, err := ddltestutil.ExtractAllTableHandles(tk.Session(), "test", "t")
	require.NoError(t, err)
	require.Equal(t, 1, len(allHandles))
	orderedHandles := testutil.MaskSortHandles(allHandles, 5, mysql.TypeLonglong)
	require.Equal(t, int64(1), orderedHandles[0])
	tk.MustExec("delete from t")

	// Test rebase auto_increment.
	tk.MustExec("alter table t auto_increment 3000000")
	tk.MustExec("insert into t values()")
	tk.MustQuery("select b from t").Check(testkit.Rows("3000000"))
	allHandles, err = ddltestutil.ExtractAllTableHandles(tk.Session(), "test", "t")
	require.NoError(t, err)
	require.Equal(t, 1, len(allHandles))
	orderedHandles = testutil.MaskSortHandles(allHandles, 5, mysql.TypeLonglong)
	require.Equal(t, int64(2), orderedHandles[0])
	tk.MustExec("delete from t")

	// Test rebase auto_random.
	tk.MustExec("alter table t auto_random_base 3000000")
	tk.MustExec("insert into t values()")
	tk.MustQuery("select b from t").Check(testkit.Rows("3000001"))
	allHandles, err = ddltestutil.ExtractAllTableHandles(tk.Session(), "test", "t")
	require.NoError(t, err)
	require.Equal(t, 1, len(allHandles))
	orderedHandles = testutil.MaskSortHandles(allHandles, 5, mysql.TypeLonglong)
	require.Equal(t, int64(3000000), orderedHandles[0])
	tk.MustExec("delete from t")

	// Test rename table.
	tk.MustExec("rename table t to t1")
	tk.MustExec("insert into t1 values()")
	res := tk.MustQuery("select b from t1")
	strInt64, err := strconv.ParseInt(res.Rows()[0][0].(string), 10, 64)
	require.NoError(t, err)
	require.Greater(t, strInt64, int64(3000002))
	allHandles, err = ddltestutil.ExtractAllTableHandles(tk.Session(), "test", "t1")
	require.NoError(t, err)
	require.Equal(t, 1, len(allHandles))
	orderedHandles = testutil.MaskSortHandles(allHandles, 5, mysql.TypeLonglong)
	require.Greater(t, orderedHandles[0], int64(3000001))
}

func TestMaxHandleAddIndex(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)

	tk.MustExec("use test")
	tk.MustExec("create table t(a bigint PRIMARY KEY, b int)")
	tk.MustExec(fmt.Sprintf("insert into t values(%v, 1)", math.MaxInt64))
	tk.MustExec(fmt.Sprintf("insert into t values(%v, 1)", math.MinInt64))
	tk.MustExec("alter table t add index idx_b(b)")
	tk.MustExec("admin check table t")

	tk.MustExec("create table t1(a bigint UNSIGNED PRIMARY KEY, b int)")
	tk.MustExec(fmt.Sprintf("insert into t1 values(%v, 1)", uint64(math.MaxUint64)))
	tk.MustExec(fmt.Sprintf("insert into t1 values(%v, 1)", 0))
	tk.MustExec("alter table t1 add index idx_b(b)")
	tk.MustExec("admin check table t1")
}

func TestSetDDLReorgWorkerCnt(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	err := ddlutil.LoadDDLReorgVars(context.Background(), tk.Session())
	require.NoError(t, err)
	require.Equal(t, int32(variable.DefTiDBDDLReorgWorkerCount), variable.GetDDLReorgWorkerCounter())
	tk.MustExec("set @@global.tidb_ddl_reorg_worker_cnt = 1")
	err = ddlutil.LoadDDLReorgVars(context.Background(), tk.Session())
	require.NoError(t, err)
	require.Equal(t, int32(1), variable.GetDDLReorgWorkerCounter())
	tk.MustExec("set @@global.tidb_ddl_reorg_worker_cnt = 100")
	err = ddlutil.LoadDDLReorgVars(context.Background(), tk.Session())
	require.NoError(t, err)
	require.Equal(t, int32(100), variable.GetDDLReorgWorkerCounter())
	_, err = tk.Exec("set @@global.tidb_ddl_reorg_worker_cnt = invalid_val")
	require.Truef(t, terror.ErrorEqual(err, variable.ErrWrongTypeForVar), "err %v", err)
	tk.MustExec("set @@global.tidb_ddl_reorg_worker_cnt = 100")
	err = ddlutil.LoadDDLReorgVars(context.Background(), tk.Session())
	require.NoError(t, err)
	require.Equal(t, int32(100), variable.GetDDLReorgWorkerCounter())
	tk.MustExec("set @@global.tidb_ddl_reorg_worker_cnt = -1")
	tk.MustQuery("SHOW WARNINGS").Check(testkit.Rows("Warning 1292 Truncated incorrect tidb_ddl_reorg_worker_cnt value: '-1'"))
	tk.MustQuery("select @@global.tidb_ddl_reorg_worker_cnt").Check(testkit.Rows("1"))

	tk.MustExec("set @@global.tidb_ddl_reorg_worker_cnt = 100")
	res := tk.MustQuery("select @@global.tidb_ddl_reorg_worker_cnt")
	res.Check(testkit.Rows("100"))

	res = tk.MustQuery("select @@global.tidb_ddl_reorg_worker_cnt")
	res.Check(testkit.Rows("100"))
	tk.MustExec("set @@global.tidb_ddl_reorg_worker_cnt = 100")
	res = tk.MustQuery("select @@global.tidb_ddl_reorg_worker_cnt")
	res.Check(testkit.Rows("100"))

	tk.MustExec("set @@global.tidb_ddl_reorg_worker_cnt = 257")
	tk.MustQuery("SHOW WARNINGS").Check(testkit.Rows("Warning 1292 Truncated incorrect tidb_ddl_reorg_worker_cnt value: '257'"))
	tk.MustQuery("select @@global.tidb_ddl_reorg_worker_cnt").Check(testkit.Rows("256"))
}

func TestSetDDLReorgBatchSize(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	err := ddlutil.LoadDDLReorgVars(context.Background(), tk.Session())
	require.NoError(t, err)
	require.Equal(t, int32(variable.DefTiDBDDLReorgBatchSize), variable.GetDDLReorgBatchSize())

	tk.MustExec("set @@global.tidb_ddl_reorg_batch_size = 1")
	tk.MustQuery("show warnings;").Check(testkit.Rows("Warning 1292 Truncated incorrect tidb_ddl_reorg_batch_size value: '1'"))
	err = ddlutil.LoadDDLReorgVars(context.Background(), tk.Session())
	require.NoError(t, err)
	require.Equal(t, variable.MinDDLReorgBatchSize, variable.GetDDLReorgBatchSize())
	tk.MustExec(fmt.Sprintf("set @@global.tidb_ddl_reorg_batch_size = %v", variable.MaxDDLReorgBatchSize+1))
	tk.MustQuery("show warnings;").Check(testkit.Rows(fmt.Sprintf("Warning 1292 Truncated incorrect tidb_ddl_reorg_batch_size value: '%d'", variable.MaxDDLReorgBatchSize+1)))
	err = ddlutil.LoadDDLReorgVars(context.Background(), tk.Session())
	require.NoError(t, err)
	require.Equal(t, variable.MaxDDLReorgBatchSize, variable.GetDDLReorgBatchSize())
	_, err = tk.Exec("set @@global.tidb_ddl_reorg_batch_size = invalid_val")
	require.True(t, terror.ErrorEqual(err, variable.ErrWrongTypeForVar), "err %v", err)
	tk.MustExec("set @@global.tidb_ddl_reorg_batch_size = 100")
	err = ddlutil.LoadDDLReorgVars(context.Background(), tk.Session())
	require.NoError(t, err)
	require.Equal(t, int32(100), variable.GetDDLReorgBatchSize())
	tk.MustExec("set @@global.tidb_ddl_reorg_batch_size = -1")
	tk.MustQuery("show warnings;").Check(testkit.Rows("Warning 1292 Truncated incorrect tidb_ddl_reorg_batch_size value: '-1'"))

	tk.MustExec("set @@global.tidb_ddl_reorg_batch_size = 100")
	res := tk.MustQuery("select @@global.tidb_ddl_reorg_batch_size")
	res.Check(testkit.Rows("100"))

	res = tk.MustQuery("select @@global.tidb_ddl_reorg_batch_size")
	res.Check(testkit.Rows(fmt.Sprintf("%v", 100)))
	tk.MustExec("set @@global.tidb_ddl_reorg_batch_size = 1000")
	res = tk.MustQuery("select @@global.tidb_ddl_reorg_batch_size")
	res.Check(testkit.Rows("1000"))
}

func TestIllegalFunctionCall4GeneratedColumns(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	// Test create an exist database
	_, err := tk.Exec("CREATE database test")
	require.Error(t, err)

	_, err = tk.Exec("create table t1 (b double generated always as (rand()) virtual);")
	require.Equal(t, dbterror.ErrGeneratedColumnFunctionIsNotAllowed.GenWithStackByArgs("b").Error(), err.Error())

	_, err = tk.Exec("create table t1 (a varchar(64), b varchar(1024) generated always as (load_file(a)) virtual);")
	require.Equal(t, dbterror.ErrGeneratedColumnFunctionIsNotAllowed.GenWithStackByArgs("b").Error(), err.Error())

	_, err = tk.Exec("create table t1 (a datetime generated always as (curdate()) virtual);")
	require.Equal(t, dbterror.ErrGeneratedColumnFunctionIsNotAllowed.GenWithStackByArgs("a").Error(), err.Error())

	_, err = tk.Exec("create table t1 (a datetime generated always as (current_time()) virtual);")
	require.Equal(t, dbterror.ErrGeneratedColumnFunctionIsNotAllowed.GenWithStackByArgs("a").Error(), err.Error())

	_, err = tk.Exec("create table t1 (a datetime generated always as (current_timestamp()) virtual);")
	require.Equal(t, dbterror.ErrGeneratedColumnFunctionIsNotAllowed.GenWithStackByArgs("a").Error(), err.Error())

	_, err = tk.Exec("create table t1 (a datetime, b varchar(10) generated always as (localtime()) virtual);")
	require.Equal(t, dbterror.ErrGeneratedColumnFunctionIsNotAllowed.GenWithStackByArgs("b").Error(), err.Error())

	_, err = tk.Exec("create table t1 (a varchar(1024) generated always as (uuid()) virtual);")
	require.Equal(t, dbterror.ErrGeneratedColumnFunctionIsNotAllowed.GenWithStackByArgs("a").Error(), err.Error())

	_, err = tk.Exec("create table t1 (a varchar(1024), b varchar(1024) generated always as (is_free_lock(a)) virtual);")
	require.Equal(t, dbterror.ErrGeneratedColumnFunctionIsNotAllowed.GenWithStackByArgs("b").Error(), err.Error())

	tk.MustExec("create table t1 (a bigint not null primary key auto_increment, b bigint, c bigint as (b + 1));")

	_, err = tk.Exec("alter table t1 add column d varchar(1024) generated always as (database());")
	require.Equal(t, dbterror.ErrGeneratedColumnFunctionIsNotAllowed.GenWithStackByArgs("d").Error(), err.Error())

	tk.MustExec("alter table t1 add column d bigint generated always as (b + 1); ")

	_, err = tk.Exec("alter table t1 modify column d bigint generated always as (connection_id());")
	require.Equal(t, dbterror.ErrGeneratedColumnFunctionIsNotAllowed.GenWithStackByArgs("d").Error(), err.Error())

	_, err = tk.Exec("alter table t1 change column c cc bigint generated always as (connection_id());")
	require.Equal(t, dbterror.ErrGeneratedColumnFunctionIsNotAllowed.GenWithStackByArgs("cc").Error(), err.Error())
}

func TestGeneratedColumnRelatedDDL(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	// Test create an exist database
	_, err := tk.Exec("CREATE database test")
	require.Error(t, err)

	_, err = tk.Exec("create table t1 (a bigint not null primary key auto_increment, b bigint as (a + 1));")
	require.Equal(t, dbterror.ErrGeneratedColumnRefAutoInc.GenWithStackByArgs("b").Error(), err.Error())

	tk.MustExec("create table t1 (a bigint not null primary key auto_increment, b bigint, c bigint as (b + 1));")

	_, err = tk.Exec("alter table t1 add column d bigint generated always as (a + 1);")
	require.Equal(t, dbterror.ErrGeneratedColumnRefAutoInc.GenWithStackByArgs("d").Error(), err.Error())

	tk.MustExec("alter table t1 add column d bigint generated always as (b + 1);")

	_, err = tk.Exec("alter table t1 modify column d bigint generated always as (a + 1);")
	require.Equal(t, dbterror.ErrGeneratedColumnRefAutoInc.GenWithStackByArgs("d").Error(), err.Error())

	// This mysql compatibility check can be disabled using tidb_enable_auto_increment_in_generated
	tk.MustExec("set session tidb_enable_auto_increment_in_generated = 1;")
	tk.MustExec("alter table t1 modify column d bigint generated always as (a + 1);")

	_, err = tk.Exec("alter table t1 add column e bigint as (z + 1);")
	require.Equal(t, dbterror.ErrBadField.GenWithStackByArgs("z", "generated column function").Error(), err.Error())

	tk.MustExec("drop table t1;")

	tk.MustExec("create table t1(a int, b int as (a+1), c int as (b+1));")
	tk.MustExec("insert into t1 (a) values (1);")
	tk.MustGetErrCode("alter table t1 modify column c int as (b+1) first;", mysql.ErrGeneratedColumnNonPrior)
	tk.MustGetErrCode("alter table t1 modify column b int as (a+1) after c;", mysql.ErrGeneratedColumnNonPrior)
	tk.MustQuery("select * from t1").Check(testkit.Rows("1 2 3"))
}

func TestSetDDLErrorCountLimit(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	err := ddlutil.LoadDDLVars(tk.Session())
	require.NoError(t, err)
	require.Equal(t, int64(variable.DefTiDBDDLErrorCountLimit), variable.GetDDLErrorCountLimit())

	tk.MustExec("set @@global.tidb_ddl_error_count_limit = -1")
	tk.MustQuery("show warnings;").Check(testkit.Rows("Warning 1292 Truncated incorrect tidb_ddl_error_count_limit value: '-1'"))
	err = ddlutil.LoadDDLVars(tk.Session())
	require.NoError(t, err)
	require.Equal(t, int64(0), variable.GetDDLErrorCountLimit())
	tk.MustExec(fmt.Sprintf("set @@global.tidb_ddl_error_count_limit = %v", uint64(math.MaxInt64)+1))
	tk.MustQuery("show warnings;").Check(testkit.Rows(fmt.Sprintf("Warning 1292 Truncated incorrect tidb_ddl_error_count_limit value: '%d'", uint64(math.MaxInt64)+1)))
	err = ddlutil.LoadDDLVars(tk.Session())
	require.NoError(t, err)
	require.Equal(t, int64(math.MaxInt64), variable.GetDDLErrorCountLimit())
	_, err = tk.Exec("set @@global.tidb_ddl_error_count_limit = invalid_val")
	require.True(t, terror.ErrorEqual(err, variable.ErrWrongTypeForVar), "err %v", err)
	tk.MustExec("set @@global.tidb_ddl_error_count_limit = 100")
	err = ddlutil.LoadDDLVars(tk.Session())
	require.NoError(t, err)
	require.Equal(t, int64(100), variable.GetDDLErrorCountLimit())
	res := tk.MustQuery("select @@global.tidb_ddl_error_count_limit")
	res.Check(testkit.Rows("100"))
}

// Test issue #9205, fix the precision problem for time type default values
// See https://github.com/pingcap/tidb/issues/9205 for details
func TestIssue9205(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(c time DEFAULT '12:12:12.8');`)
	tk.MustQuery("show create table `t`").Check(testkit.RowsWithSep("|",
		""+
			"t CREATE TABLE `t` (\n"+
			"  `c` time DEFAULT '12:12:13'\n"+
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin",
	))
	tk.MustExec(`alter table t add column c1 time default '12:12:12.000000';`)
	tk.MustQuery("show create table `t`").Check(testkit.RowsWithSep("|",
		""+
			"t CREATE TABLE `t` (\n"+
			"  `c` time DEFAULT '12:12:13',\n"+
			"  `c1` time DEFAULT '12:12:12'\n"+
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin",
	))

	tk.MustExec(`alter table t alter column c1 set default '2019-02-01 12:12:10.4';`)
	tk.MustQuery("show create table `t`").Check(testkit.RowsWithSep("|",
		""+
			"t CREATE TABLE `t` (\n"+
			"  `c` time DEFAULT '12:12:13',\n"+
			"  `c1` time DEFAULT '12:12:10'\n"+
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin",
	))

	tk.MustExec(`alter table t modify c1 time DEFAULT '770:12:12.000000';`)
	tk.MustQuery("show create table `t`").Check(testkit.RowsWithSep("|",
		""+
			"t CREATE TABLE `t` (\n"+
			"  `c` time DEFAULT '12:12:13',\n"+
			"  `c1` time DEFAULT '770:12:12'\n"+
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin",
	))
}

func TestCheckDefaultFsp(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec(`drop table if exists t;`)

	_, err := tk.Exec("create table t (  tt timestamp default now(1));")
	require.EqualError(t, err, "[ddl:1067]Invalid default value for 'tt'")

	_, err = tk.Exec("create table t (  tt timestamp(1) default current_timestamp);")
	require.EqualError(t, err, "[ddl:1067]Invalid default value for 'tt'")

	_, err = tk.Exec("create table t (  tt timestamp(1) default now(2));")
	require.EqualError(t, err, "[ddl:1067]Invalid default value for 'tt'")

	tk.MustExec("create table t (  tt timestamp(1) default now(1));")
	tk.MustExec("create table t2 (  tt timestamp default current_timestamp());")
	tk.MustExec("create table t3 (  tt timestamp default current_timestamp(0));")

	_, err = tk.Exec("alter table t add column ttt timestamp default now(2);")
	require.EqualError(t, err, "[ddl:1067]Invalid default value for 'ttt'")

	_, err = tk.Exec("alter table t add column ttt timestamp(5) default current_timestamp;")
	require.EqualError(t, err, "[ddl:1067]Invalid default value for 'ttt'")

	_, err = tk.Exec("alter table t add column ttt timestamp(5) default now(2);")
	require.EqualError(t, err, "[ddl:1067]Invalid default value for 'ttt'")

	_, err = tk.Exec("alter table t modify column tt timestamp(1) default now();")
	require.EqualError(t, err, "[ddl:1067]Invalid default value for 'tt'")

	_, err = tk.Exec("alter table t modify column tt timestamp(4) default now(5);")
	require.EqualError(t, err, "[ddl:1067]Invalid default value for 'tt'")

	_, err = tk.Exec("alter table t change column tt tttt timestamp(4) default now(5);")
	require.EqualError(t, err, "[ddl:1067]Invalid default value for 'tttt'")

	_, err = tk.Exec("alter table t change column tt tttt timestamp(1) default now();")
	require.EqualError(t, err, "[ddl:1067]Invalid default value for 'tttt'")
}

func TestTimestampMinDefaultValue(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists tdv;")
	tk.MustExec("create table tdv(a int);")
	tk.MustExec("ALTER TABLE tdv ADD COLUMN ts timestamp DEFAULT '1970-01-01 08:00:01';")
}

// this test will change the fail-point `mockAutoIDChange`, so we move it to the `testRecoverTable` suite
func TestRenameTable(t *testing.T) {
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/meta/autoid/mockAutoIDChange", `return(true)`))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/meta/autoid/mockAutoIDChange"))
	}()
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)

	tk.MustExec("drop database if exists rename1")
	tk.MustExec("drop database if exists rename2")
	tk.MustExec("drop database if exists rename3")

	tk.MustExec("create database rename1")
	tk.MustExec("create database rename2")
	tk.MustExec("create database rename3")
	tk.MustExec("create table rename1.t (a int primary key auto_increment)")
	tk.MustExec("insert rename1.t values ()")
	tk.MustExec("rename table rename1.t to rename2.t")
	// Make sure the drop old database doesn't affect the rename3.t's operations.
	tk.MustExec("drop database rename1")
	tk.MustExec("insert rename2.t values ()")
	tk.MustExec("rename table rename2.t to rename3.t")
	tk.MustExec("insert rename3.t values ()")
	tk.MustQuery("select * from rename3.t").Check(testkit.Rows("1", "5001", "10001"))
	// Make sure the drop old database doesn't affect the rename3.t's operations.
	tk.MustExec("drop database rename2")
	tk.MustExec("insert rename3.t values ()")
	tk.MustQuery("select * from rename3.t").Check(testkit.Rows("1", "5001", "10001", "10002"))
	tk.MustExec("drop database rename3")

	tk.MustExec("create database rename1")
	tk.MustExec("create database rename2")
	tk.MustExec("create table rename1.t (a int primary key auto_increment)")
	tk.MustExec("rename table rename1.t to rename2.t1")
	tk.MustExec("insert rename2.t1 values ()")
	result := tk.MustQuery("select * from rename2.t1")
	result.Check(testkit.Rows("1"))
	// Make sure the drop old database doesn't affect the t1's operations.
	tk.MustExec("drop database rename1")
	tk.MustExec("insert rename2.t1 values ()")
	result = tk.MustQuery("select * from rename2.t1")
	result.Check(testkit.Rows("1", "2"))
	// Rename a table to another table in the same database.
	tk.MustExec("rename table rename2.t1 to rename2.t2")
	tk.MustExec("insert rename2.t2 values ()")
	result = tk.MustQuery("select * from rename2.t2")
	result.Check(testkit.Rows("1", "2", "5001"))
	tk.MustExec("drop database rename2")

	tk.MustExec("create database rename1")
	tk.MustExec("create database rename2")
	tk.MustExec("create table rename1.t (a int primary key auto_increment)")
	tk.MustExec("insert rename1.t values ()")
	tk.MustExec("rename table rename1.t to rename2.t1")
	// Make sure the value is greater than autoid.step.
	tk.MustExec("insert rename2.t1 values (100000)")
	tk.MustExec("insert rename2.t1 values ()")
	result = tk.MustQuery("select * from rename2.t1")
	result.Check(testkit.Rows("1", "100000", "100001"))
	_, err := tk.Exec("insert rename1.t values ()")
	require.Error(t, err)
	tk.MustExec("drop database rename1")
	tk.MustExec("drop database rename2")
}

func TestAutoIncrementColumnErrorMessage(t *testing.T) {
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	// Test create an exist database
	_, err := tk.Exec("CREATE database test")
	require.Error(t, err)

	tk.MustExec("CREATE TABLE t1 (t1_id INT NOT NULL AUTO_INCREMENT PRIMARY KEY);")

	_, err = tk.Exec("CREATE INDEX idx1 ON t1 ((t1_id + t1_id));")
	require.Equal(t, dbterror.ErrExpressionIndexCanNotRefer.GenWithStackByArgs("idx1").Error(), err.Error())

	// This mysql compatibility check can be disabled using tidb_enable_auto_increment_in_generated
	tk.MustExec("SET SESSION tidb_enable_auto_increment_in_generated = 1;")
	tk.MustExec("CREATE INDEX idx1 ON t1 ((t1_id + t1_id));")
}

func TestRenameMultiTables(t *testing.T) {
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/meta/autoid/mockAutoIDChange", `return(true)`))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/meta/autoid/mockAutoIDChange"))
	}()
	store, clean := testkit.CreateMockStore(t)
	defer clean()
	tk := testkit.NewTestKit(t, store)

	tk.MustExec("drop database if exists rename1")
	tk.MustExec("drop database if exists rename2")
	tk.MustExec("drop database if exists rename3")
	tk.MustExec("drop database if exists rename4")

	tk.MustExec("create database rename1")
	tk.MustExec("create database rename2")
	tk.MustExec("create database rename3")
	tk.MustExec("create database rename4")
	tk.MustExec("create table rename1.t1 (a int primary key auto_increment)")
	tk.MustExec("create table rename3.t3 (a int primary key auto_increment)")
	tk.MustExec("insert rename1.t1 values ()")
	tk.MustExec("insert rename3.t3 values ()")
	tk.MustExec("rename table rename1.t1 to rename2.t2, rename3.t3 to rename4.t4")
	// Make sure the drop old database doesn't affect t2,t4's operations.
	tk.MustExec("drop database rename1")
	tk.MustExec("insert rename2.t2 values ()")
	tk.MustExec("drop database rename3")
	tk.MustExec("insert rename4.t4 values ()")
	tk.MustQuery("select * from rename2.t2").Check(testkit.Rows("1", "5001"))
	tk.MustQuery("select * from rename4.t4").Check(testkit.Rows("1", "5001"))
	// Rename a table to another table in the same database.
	tk.MustExec("rename table rename2.t2 to rename2.t1, rename4.t4 to rename4.t3")
	tk.MustExec("insert rename2.t1 values ()")
	tk.MustQuery("select * from rename2.t1").Check(testkit.Rows("1", "5001", "10001"))
	tk.MustExec("insert rename4.t3 values ()")
	tk.MustQuery("select * from rename4.t3").Check(testkit.Rows("1", "5001", "10001"))
	tk.MustExec("drop database rename2")
	tk.MustExec("drop database rename4")

	tk.MustExec("create database rename1")
	tk.MustExec("create database rename2")
	tk.MustExec("create database rename3")
	tk.MustExec("create table rename1.t1 (a int primary key auto_increment)")
	tk.MustExec("create table rename3.t3 (a int primary key auto_increment)")
	tk.MustGetErrCode("rename table rename1.t1 to rename2.t2, rename3.t3 to rename2.t2", errno.ErrTableExists)
	tk.MustExec("rename table rename1.t1 to rename2.t2, rename2.t2 to rename1.t1")
	tk.MustExec("rename table rename1.t1 to rename2.t2, rename3.t3 to rename1.t1")
	tk.MustExec("use rename1")
	tk.MustQuery("show tables").Check(testkit.Rows("t1"))
	tk.MustExec("use rename2")
	tk.MustQuery("show tables").Check(testkit.Rows("t2"))
	tk.MustExec("use rename3")
	tk.MustExec("create table rename3.t3 (a int primary key auto_increment)")
	tk.MustGetErrCode("rename table rename1.t1 to rename1.t2, rename1.t1 to rename3.t3", errno.ErrTableExists)
	tk.MustGetErrCode("rename table rename1.t1 to rename1.t2, rename1.t1 to rename3.t4", errno.ErrNoSuchTable)
	tk.MustExec("drop database rename1")
	tk.MustExec("drop database rename2")
	tk.MustExec("drop database rename3")
}
