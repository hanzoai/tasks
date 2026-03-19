package cassandra

import (
	"github.com/gocql/gocql"
	"github.com/hanzoai/tasks/common/config"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/metrics"
	commongocql "github.com/hanzoai/tasks/common/persistence/nosql/nosqlplugin/cassandra/gocql"
	"github.com/hanzoai/tasks/common/persistence/schema"
	"github.com/hanzoai/tasks/common/resolver"
	cassandraschema "github.com/hanzoai/tasks/schema/cassandra"
)

// VerifyCompatibleVersion ensures that the installed version of temporal and visibility keyspaces
// is greater than or equal to the expected version.
// In most cases, the versions should match. However if after a schema upgrade there is a code
// rollback, the code version (expected version) would fall lower than the actual version in
// cassandra.
func VerifyCompatibleVersion(
	cfg config.Persistence,
	r resolver.ServiceResolver,
	logger log.Logger,
) error {
	return checkMainKeyspace(cfg, r, logger)
}

func checkMainKeyspace(
	cfg config.Persistence,
	r resolver.ServiceResolver,
	logger log.Logger,
) error {
	ds, ok := cfg.DataStores[cfg.DefaultStore]
	if ok && ds.Cassandra != nil {
		return CheckCompatibleVersion(*ds.Cassandra, r, cassandraschema.Version, logger)
	}
	return nil
}

// CheckCompatibleVersion check the version compatibility
func CheckCompatibleVersion(
	cfg config.Cassandra,
	r resolver.ServiceResolver,
	expectedVersion string,
	logger log.Logger,
) error {

	session, err := commongocql.NewSession(
		func() (*gocql.ClusterConfig, error) {
			return commongocql.NewCassandraCluster(cfg, r)
		},
		logger,
		metrics.NoopMetricsHandler,
	)
	if err != nil {
		return err
	}
	defer session.Close()

	schemaVersionReader := NewSchemaVersionReader(session)

	return schema.VerifyCompatibleVersion(schemaVersionReader, cfg.Keyspace, expectedVersion)
}
