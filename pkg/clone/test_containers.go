package clone

import (
	"context"
	"os"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// yep, globals, we should only have one container per test run
var vitessContainer *DatabaseContainer
var tidbContainer *DatabaseContainer

func createSchema(config DBConfig, database string) error {
	db, err := config.DB()
	if err != nil {
		return errors.WithStack(err)
	}
	if config.Type != Vitess {
		_, err = db.Exec("CREATE DATABASE " + database)
		if err != nil {
			return errors.WithStack(err)
		}
	}

	config.Database = database
	db, err = config.DB()
	if err != nil {
		return errors.WithStack(err)
	}
	_, err = db.Exec(`
		CREATE TABLE customers (
		  id BIGINT(20) NOT NULL AUTO_INCREMENT,
		  name VARCHAR(255) NOT NULL,
		  PRIMARY KEY (id)
		)
	`)
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func insertBaseData(config DBConfig) error {
	db, err := config.DB()
	if err != nil {
		return errors.WithStack(err)
	}
	_, err = db.Exec(`
		INSERT INTO customers (name) VALUES (?)
	`, "Jon Tirsen")
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

type DatabaseContainer struct {
	pool     *dockertest.Pool
	resource *dockertest.Resource
	config   DBConfig
}

func (c *DatabaseContainer) Close() error {
	return c.pool.Purge(c.resource)
}

func (c *DatabaseContainer) Config() DBConfig {
	return c.config
}

func startVitess() error {
	if vitessContainer != nil {
		vitessContainer.Close()
	}

	log.Debugf("starting Vitess")
	// uses a sensible default on windows (tcp/http) and linux/osx (socket)
	pool, err := dockertest.NewPool("")
	if err != nil {
		return errors.WithStack(err)
	}

	// pulls an image, creates a container based on it and runs it
	path, err := os.Getwd()
	if err != nil {
		return errors.WithStack(err)
	}
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "vitess/base",
		Tag:        "v7.0.2",
		Cmd: []string{
			"vttestserver",
			"-port=15000",
			"-mysql_bind_host=0.0.0.0",
			"-keyspaces=main,customer",
			"-data_dir=/vt/vtdataroot",
			"-schema_dir=schema",
			"-num_shards=1,2",
		},
		ExposedPorts: []string{"15000", "15001", "15002", "15003"},
		Mounts: []string{
			path + "/../../test/schema:/vt/src/vitess.io/vitess/schema",
		},
	})
	if err != nil {
		return errors.WithStack(err)
	}
	err = resource.Expire(15 * 60)
	if err != nil {
		_ = pool.Purge(resource)
		return errors.WithStack(err)
	}

	config := DBConfig{
		Type:     Vitess,
		Host:     "localhost:" + resource.GetPort("15001/tcp"),
		Username: "vt_dba",
		Password: "",
		Database: "customer@master",
	}

	// exponential backoff-retry, because the application in the container might not be ready to accept connections yet
	time.Sleep(1 * time.Second)
	if err := pool.Retry(func() error {
		var err error
		db, err := config.DB()
		if err != nil {
			return errors.WithStack(err)
		}
		err = db.Ping()
		if err != nil {
			return errors.WithStack(err)
		}
		ctx := context.Background()
		conn, err := db.Conn(ctx)
		if err != nil {
			return errors.WithStack(err)
		}
		rows, err := conn.QueryContext(ctx, "SELECT * FROM customers")
		if err != nil {
			return errors.WithStack(err)
		}
		rows.Next()
		return nil
	}); err != nil {
		_ = pool.Purge(resource)
		return errors.WithStack(err)
	}

	err = insertBaseData(config)
	if err != nil {
		_ = pool.Purge(resource)
		return errors.WithStack(err)
	}

	vitessContainer = &DatabaseContainer{pool: pool, resource: resource, config: config}

	return nil
}

func startTidb() error {
	if tidbContainer != nil {
		tidbContainer.Close()
	}

	log.Debugf("starting TiDB")
	// uses a sensible default on windows (tcp/http) and linux/osx (socket)
	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Fatalf("Could not connect to docker: %s", err)
	}

	// pulls an image, creates a container based on it and runs it
	resource, err := pool.Run("pingcap/tidb", "latest", []string{})
	if err != nil {
		return errors.WithStack(err)
	}
	err = resource.Expire(15 * 60)
	if err != nil {
		_ = pool.Purge(resource)
		return errors.WithStack(err)
	}

	config := DBConfig{
		Type:     MySQL,
		Host:     "localhost:" + resource.GetPort("4000/tcp"),
		Username: "root",
		Password: "",
		Database: "mysql",
	}

	// exponential backoff-retry, because the application in the container might not be ready to accept connections yet
	if err := pool.Retry(func() error {
		var err error
		db, err := config.DB()
		if err != nil {
			return errors.WithStack(err)
		}
		return db.Ping()
	}); err != nil {
		_ = pool.Purge(resource)
		return errors.WithStack(err)
	}

	err = createSchema(config, "mydatabase")
	if err != nil {
		_ = pool.Purge(resource)
		return errors.WithStack(err)
	}

	config.Database = "mydatabase"

	err = insertBaseData(config)
	if err != nil {
		_ = pool.Purge(resource)
		return errors.WithStack(err)
	}

	tidbContainer = &DatabaseContainer{pool: pool, resource: resource, config: config}

	return nil
}

// startAll (re)starts both Vitess and TiDB in parallel
func startAll() error {
	g, _ := errgroup.WithContext(context.Background())
	g.Go(startVitess)
	g.Go(startTidb)
	return g.Wait()
}
