module github.com/hanzoai/tasks

go 1.26.1

retract (
	v1.30.0
	v1.26.1 // Contains retractions only.
	v1.26.0 // Published accidentally.
)

require (
	github.com/luxfi/log v1.4.1
	github.com/robfig/cron/v3 v3.0.1
	golang.org/x/time v0.10.0
)

require (
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/grandcat/zeroconf v1.0.0 // indirect
	github.com/luxfi/mdns v0.1.0 // indirect
	github.com/miekg/dns v1.1.62 // indirect
	golang.org/x/mod v0.33.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/tools v0.42.0 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)

require (
	github.com/luxfi/zap v0.2.1
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
)
