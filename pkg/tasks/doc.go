// Package tasks provides the Hanzo Tasks client for Go applications.
//
// Two methods, two use cases:
//
//	client := tasks.New(os.Getenv("TASKS_ZAP"), nil)
//	client.Add("settlement.process", "30s", fn)   // recurring schedule (duration)
//	client.Add("audit.archive", "0 3 * * *", fn)  // recurring schedule (cron)
//	client.Now("webhook.deliver", payload)         // fire once immediately
//
// Transport: ZAP (binary, low-latency) or local goroutine.
// When TASKS_ZAP is set, tasks submit durably over the ZAP binary protocol.
// When it is empty, tasks run locally via goroutine timers (dev mode).
//
// Integration with Hanzo Base:
//
//	app.Tasks().Add("cleanup", "1h", fn)
//	app.Tasks().Now("email.send", payload)
package tasks
