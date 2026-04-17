// Package sdk provides the Hanzo Tasks client for Go applications.
//
// Two methods, two use cases:
//
//	client := sdk.New(os.Getenv("TASKS_URL"), nil)
//	client.Add("settlement.process", "30s", fn)   // recurring schedule
//	client.Now("webhook.deliver", payload)          // fire once immediately
//
// When TASKS_URL is set, tasks execute as durable Temporal workflows
// with retry, dead letter, and audit trail. When empty, tasks run
// locally via goroutine timers (dev mode).
//
// Integration with Hanzo Base:
//
//	app.Tasks().Add("cleanup", "1h", fn)
//	app.Tasks().Now("email.send", payload)
package tasks
