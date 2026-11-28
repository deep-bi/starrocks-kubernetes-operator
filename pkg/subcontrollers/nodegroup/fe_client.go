package nodegroup

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	_ "github.com/go-sql-driver/mysql" // import mysql driver
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/StarRocks/starrocks-kubernetes-operator/cmd/config"
	srapi "github.com/StarRocks/starrocks-kubernetes-operator/pkg/apis/starrocks/v1"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/common/mysqlutil"
	rutils "github.com/StarRocks/starrocks-kubernetes-operator/pkg/common/resource_utils"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils/templates/service"
)

const frontendRoleObserver = "OBSERVER"

// Node is a compute node or a frontend as the FE lists it.
type Node struct {
	// Host is the FQDN the node registered with.
	Host string
	// Port is the port the node registered with: the heartbeat port of a compute node, the edit log port of
	// a frontend.
	Port string
	// Alive reports whether the FE reaches the node.
	Alive bool
}

// FEClient is what a node group asks of the FE of its cluster.
type FEClient interface {
	// Nodes lists the nodes of a role the FE knows: every compute node, or every observer.
	Nodes(ctx context.Context, role srapi.NodeGroupRole) ([]Node, error)
	// Drop removes a node of a role from the FE.
	Drop(ctx context.Context, role srapi.NodeGroupRole, node Node) error
}

// NewFEClientFunc builds the FEClient of one reconcile. envVars are the environment variables of the pods of
// the group, which is where the root password (MYSQL_PWD) is read from.
type NewFEClientFunc func(ctx context.Context, k8sClient client.Client, cluster *srapi.StarRocksCluster,
	envVars []corev1.EnvVar, feConfig map[string]interface{}) FEClient

type mysqlFEClient struct {
	dsn string
}

// NewMySQLFEClient is the production NewFEClientFunc: it talks to the FE service of the cluster over MySQL.
func NewMySQLFEClient(ctx context.Context, k8sClient client.Client, cluster *srapi.StarRocksCluster,
	envVars []corev1.EnvVar, feConfig map[string]interface{}) FEClient {
	logger := logr.FromContextOrDiscard(ctx)
	rootPassword := ""
	for _, envVar := range envVars {
		if envVar.Name != mysqlPasswordEnv {
			continue
		}
		value, err := k8sutils.GetEnvVarValue(ctx, k8sClient, cluster.Namespace, envVar)
		if err != nil {
			logger.Error(err, "failed to get MYSQL_PWD from env vars, use the default password: empty string")
			break
		}
		rootPassword = value
	}
	host := service.ExternalServiceName(cluster.Name, (*srapi.StarRocksFeSpec)(nil)) + "." + cluster.Namespace
	port := strconv.Itoa(int(rutils.GetPort(feConfig, rutils.QUERY_PORT)))
	return &mysqlFEClient{dsn: mysqlutil.DSN(rootPassword, host, port, config.FeSslMode)}
}

func (c *mysqlFEClient) Nodes(ctx context.Context, role srapi.NodeGroupRole) ([]Node, error) {
	ctx, cancel := mysqlutil.WithRequestTimeout(ctx)
	defer cancel()

	db, err := sql.Open("mysql", c.dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	statement, portColumn := "SHOW COMPUTE NODES", "HeartbeatPort"
	if role == srapi.NodeGroupRoleFEObserver {
		statement, portColumn = "SHOW FRONTENDS", "EditLogPort"
	}
	rows, err := db.QueryContext(ctx, statement)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return parseNodes(rows, portColumn, role == srapi.NodeGroupRoleFEObserver)
}

// parseNodes reads rows by column name, so that column order and columns added by newer FE versions do not
// matter. With observersOnly, frontends of any other role are left out.
func parseNodes(rows *sql.Rows, portColumn string, observersOnly bool) ([]Node, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var nodes []Node
	for rows.Next() {
		values := make([]sql.RawBytes, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		if err = rows.Scan(valuePtrs...); err != nil {
			return nil, err
		}
		node := Node{}
		role := ""
		for i, col := range columns {
			switch col {
			case "IP":
				node.Host = string(values[i])
			case portColumn:
				node.Port = string(values[i])
			case "Alive":
				node.Alive = strings.EqualFold(string(values[i]), "true")
			case "Role":
				role = string(values[i])
			}
		}
		if observersOnly && !strings.EqualFold(role, frontendRoleObserver) {
			continue
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func (c *mysqlFEClient) Drop(ctx context.Context, role srapi.NodeGroupRole, node Node) error {
	ctx, cancel := mysqlutil.WithRequestTimeout(ctx)
	defer cancel()

	db, err := sql.Open("mysql", c.dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	kind := "COMPUTE NODE"
	if role == srapi.NodeGroupRoleFEObserver {
		kind = "OBSERVER"
	}
	_, err = db.ExecContext(ctx, fmt.Sprintf("ALTER SYSTEM DROP %s \"%s:%s\"", kind, node.Host, node.Port))
	return err
}
