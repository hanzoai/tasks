module github.com/hanzoai/tasks

go 1.26.1

retract (
	v1.30.0
	v1.26.1 // Contains retractions only.
	v1.26.0 // Published accidentally.
)

require (
	github.com/luxfi/database v1.18.1
	github.com/luxfi/log v1.4.1
	github.com/robfig/cron/v3 v3.0.1
	golang.org/x/time v0.10.0
)

require (
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/gorilla/rpc v1.2.1 // indirect
	github.com/grandcat/zeroconf v1.0.0 // indirect
	github.com/klauspost/compress v1.18.4 // indirect
	github.com/luxfi/cache v1.2.1 // indirect
	github.com/luxfi/compress v0.0.5 // indirect
	github.com/luxfi/concurrent v0.0.3 // indirect
	github.com/luxfi/crypto v1.17.39 // indirect
	github.com/luxfi/ids v1.2.9 // indirect
	github.com/luxfi/math v1.2.3 // indirect
	github.com/luxfi/math/big v0.1.0 // indirect
	github.com/luxfi/mdns v0.1.0 // indirect
	github.com/luxfi/metric v1.5.0 // indirect
	github.com/luxfi/mock v0.1.1 // indirect
	github.com/miekg/dns v1.1.62 // indirect
	github.com/mr-tron/base58 v1.2.0 // indirect
	go.uber.org/mock v0.6.0 // indirect
	golang.org/x/crypto v0.49.0 // indirect
	golang.org/x/exp v0.0.0-20260212183809-81e46e3db34a // indirect
	golang.org/x/mod v0.33.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/tools v0.42.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)

require (
	github.com/luxfi/zap v0.2.1
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
)
