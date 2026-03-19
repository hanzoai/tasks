package tests

import (
	"net"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/hanzoai/tasks/common/config"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/metrics"
	"github.com/hanzoai/tasks/common/metrics/metricstest"
	p "github.com/hanzoai/tasks/common/persistence"
	"github.com/hanzoai/tasks/common/persistence/serialization"
	"github.com/hanzoai/tasks/common/persistence/sql"
	"github.com/hanzoai/tasks/common/persistence/sql/sqlplugin"
	"github.com/hanzoai/tasks/common/persistence/sql/sqlplugin/mysql"
	"github.com/hanzoai/tasks/common/resolver"
	"github.com/hanzoai/tasks/common/shuffle"
	"github.com/hanzoai/tasks/temporal/environment"
	"go.uber.org/zap/zaptest"
)

// TODO merge the initialization with existing persistence setup
const (
	testMySQLClusterName = "temporal_mysql_cluster"

	testMySQLUser               = "temporal"
	testMySQLPassword           = "temporal"
	testMySQLConnectionProtocol = "tcp"
	testMySQLDatabaseNamePrefix = "test_"
	testMySQLDatabaseNameSuffix = "temporal_persistence"

	// TODO hard code this dir for now
	//  need to merge persistence test config / initialization in one place
	testMySQLExecutionSchema  = "../../../schema/mysql/v8/temporal/schema.sql"
	testMySQLVisibilitySchema = "../../../schema/mysql/v8/visibility/schema.sql"
)

type (
	MySQLTestData struct {
		Cfg     *config.SQL
		Factory *sql.Factory
		Logger  log.Logger
		Metrics *metricstest.Capture
	}
)

func setUpMySQLTest(t *testing.T) (MySQLTestData, func()) {
	var testData MySQLTestData
	testData.Cfg = NewMySQLConfig()
	testData.Logger = log.NewZapLogger(zaptest.NewLogger(t))
	mh := metricstest.NewCaptureHandler()
	testData.Metrics = mh.StartCapture()
	SetupMySQLDatabase(t, testData.Cfg)
	SetupMySQLSchema(t, testData.Cfg)

	testData.Factory = sql.NewFactory(
		*testData.Cfg,
		resolver.NewNoopResolver(),
		testMySQLClusterName,
		testData.Logger,
		mh,
		serialization.NewSerializer(),
	)

	tearDown := func() {
		testData.Factory.Close()
		mh.StopCapture(testData.Metrics)
		TearDownMySQLDatabase(t, testData.Cfg)
	}

	return testData, tearDown
}

// NewMySQLConfig returns a new MySQL config for test
func NewMySQLConfig() *config.SQL {
	return &config.SQL{
		User:     testMySQLUser,
		Password: testMySQLPassword,
		ConnectAddr: net.JoinHostPort(
			environment.GetMySQLAddress(),
			strconv.Itoa(environment.GetMySQLPort()),
		),
		ConnectProtocol: testMySQLConnectionProtocol,
		PluginName:      mysql.PluginName,
		DatabaseName:    testMySQLDatabaseNamePrefix + shuffle.String(testMySQLDatabaseNameSuffix),
	}
}

func SetupMySQLDatabase(t *testing.T, cfg *config.SQL) {
	adminCfg := *cfg
	// NOTE need to connect with empty name to create new database
	adminCfg.DatabaseName = ""

	db, err := sql.NewSQLAdminDB(sqlplugin.DbKindUnknown, &adminCfg, resolver.NewNoopResolver(), log.NewTestLogger(), metrics.NoopMetricsHandler)
	if err != nil {
		t.Fatalf("unable to create MySQL admin DB: %v", err)
	}
	defer func() { _ = db.Close() }()

	err = db.CreateDatabase(cfg.DatabaseName)
	if err != nil {
		t.Fatalf("unable to create MySQL database: %v", err)
	}
}

func SetupMySQLSchema(t *testing.T, cfg *config.SQL) {
	db, err := sql.NewSQLAdminDB(sqlplugin.DbKindUnknown, cfg, resolver.NewNoopResolver(), log.NewTestLogger(), metrics.NoopMetricsHandler)
	if err != nil {
		t.Fatalf("unable to create MySQL admin DB: %v", err)
	}
	defer func() { _ = db.Close() }()

	schemaPath, err := filepath.Abs(testMySQLExecutionSchema)
	if err != nil {
		t.Fatal(err)
	}

	statements, err := p.LoadAndSplitQuery([]string{schemaPath})
	if err != nil {
		t.Fatal(err)
	}

	for _, stmt := range statements {
		if err = db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	schemaPath, err = filepath.Abs(testMySQLVisibilitySchema)
	if err != nil {
		t.Fatal(err)
	}

	statements, err = p.LoadAndSplitQuery([]string{schemaPath})
	if err != nil {
		t.Fatal(err)
	}

	for _, stmt := range statements {
		if err = db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

func TearDownMySQLDatabase(t *testing.T, cfg *config.SQL) {
	adminCfg := *cfg
	// NOTE need to connect with empty name to create new database
	adminCfg.DatabaseName = ""

	db, err := sql.NewSQLAdminDB(sqlplugin.DbKindUnknown, &adminCfg, resolver.NewNoopResolver(), log.NewTestLogger(), metrics.NoopMetricsHandler)
	if err != nil {
		t.Fatalf("unable to create MySQL admin DB: %v", err)
	}
	defer func() { _ = db.Close() }()

	err = db.DropDatabase(cfg.DatabaseName)
	if err != nil {
		t.Fatalf("unable to drop MySQL database: %v", err)
	}
}
