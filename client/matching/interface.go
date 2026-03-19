package matching

import "github.com/hanzoai/tasks/common/tqid"

type RoutingClient interface {
	Route(p tqid.Partition) (string, error)
}
