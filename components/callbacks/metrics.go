package callbacks

import (
	chasmcallbacks "github.com/hanzoai/tasks/chasm/lib/callback"
)

var (
	RequestCounter          = chasmcallbacks.RequestCounter
	RequestLatencyHistogram = chasmcallbacks.RequestLatencyHistogram
)
