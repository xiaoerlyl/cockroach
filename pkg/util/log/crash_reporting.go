// Copyright 2017 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package log

import (
	"fmt"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"time"

	raven "github.com/getsentry/raven-go"
	"github.com/pkg/errors"
	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/util/caller"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
)

var (
	// DiagnosticsReportingEnabled wraps "diagnostics.reporting.enabled".
	//
	// "diagnostics.reporting.enabled" enables reporting of metrics related to a
	// node's storage (number, size and health of ranges) back to CockroachDB.
	// Collecting this data from production clusters helps us understand and improve
	// how our storage systems behave in real-world use cases.
	//
	// Note: while the setting itself is actually defined with a default value of
	// `false`, it is usually automatically set to `true` when a cluster is created
	// (or is migrated from a earlier beta version). This can be prevented with the
	// env var COCKROACH_SKIP_ENABLING_DIAGNOSTIC_REPORTING.
	//
	// Doing this, rather than just using a default of `true`, means that a node
	// will not errantly send a report using a default before loading settings.
	DiagnosticsReportingEnabled = settings.RegisterBoolSetting(
		"diagnostics.reporting.enabled",
		"enable reporting diagnostic metrics to cockroach labs",
		false,
	)

	// CrashReports wraps "diagnostics.reporting.send_crash_reports".
	CrashReports = settings.RegisterBoolSetting(
		"diagnostics.reporting.send_crash_reports",
		"send crash and panic reports",
		true,
	)

	// startTime records when the process started so that crash reports can
	// include the server's uptime as an extra tag.
	startTime = time.Now()
)

// TODO(dt): this should be split from the report interval.
// var statsResetFrequency = settings.RegisterDurationSetting(
// 	"sql.metrics.statement_details.reset_interval",
// 	"interval at which the collected statement statistics should be reset",
// 	time.Hour,
// )

// ReportingSettingsSingleton is a global that holds a reference to the
// setting.Values of an active instance of cluster.Settings. In a regular
// running node, there is exactly one such instance. In multi-node testing,
// there may be many, and so it is not clear which one will win out, but that is
// acceptable.
//
// The global is a compromise to free callers of log.Fatal{,f} from having to
// provide their cluster's settings on every call.
var ReportingSettingsSingleton atomic.Value // stores *setting.Values

// RecoverAndReportPanic can be invoked on goroutines that run with
// stderr redirected to logs to ensure the user gets informed on the
// real stderr a panic has occurred.
func RecoverAndReportPanic(ctx context.Context, sv *settings.Values) {
	if r := recover(); r != nil {
		ReportPanic(ctx, sv, r, 1)
		panic(r)
	}
}

// A Safe panic can be reported verbatim, i.e. does not leak information.
//
// TODO(dt): flesh out, see #15892.
type Safe struct {
	V interface{}
}

func format(r interface{}) string {
	switch wrapped := r.(type) {
	case *Safe:
		return fmt.Sprintf("%+v", wrapped.V)
	case Safe:
		return fmt.Sprintf("%+v", wrapped.V)
	}
	return fmt.Sprintf("%T", r)
}

// ReportPanic reports a panic has occurred on the real stderr.
func ReportPanic(ctx context.Context, sv *settings.Values, r interface{}, depth int) {
	Shout(ctx, Severity_ERROR, "a panic has occurred!")

	// TODO(dt,knz,sql-team): we need to audit all sprintf'ing of values into the
	// errors and strings passed to panic, to ensure raw user data is kept
	// separate and can thus be elided here. For now, the type is about all we can
	// assume is safe to report, which combined with file and line info should be
	// at least somewhat helpful in telling us where crashes are coming from.
	// We capture the full stacktrace below, so we only need the short file and
	// line here help uniquely identify the error.
	// Some exceptions, like a runtime.Error, are assumed to be fine as-is.

	var reportable interface{}
	switch r.(type) {
	case runtime.Error:
		reportable = r
	default:
		file, line, _ := caller.Lookup(depth + 3)
		reportable = fmt.Sprintf("%s %s:%d", format(r), filepath.Base(file), line)
	}
	sendCrashReport(ctx, sv, reportable, depth+3)

	// Ensure that the logs are flushed before letting a panic
	// terminate the server.
	Flush()

	// We do not use Shout() to print the panic details here, because
	// if stderr is not redirected (e.g. when logging to file is
	// disabled) Shout() would copy its argument to stderr
	// unconditionally, and we don't want that: Go's runtime system
	// already unconditonally copies the panic details to stderr.
	// Instead, we copy manually the details to stderr, only when stderr
	// is redirected to a file otherwise.
	if stderrRedirected {
		fmt.Fprintf(OrigStderr, "%v\n\n%s\n", r, debug.Stack())
	}
}

var crashReportURL = func() string {
	var defaultURL string
	if build.IsRelease() {
		defaultURL = "https://ignored:ignored@errors.cockroachdb.com/sentry"
	}
	return envutil.EnvOrDefaultString("COCKROACH_CRASH_REPORTS", defaultURL)
}()

// SetupCrashReporter sets the crash reporter info.
func SetupCrashReporter(ctx context.Context, cmd string) {
	if err := raven.SetDSN(crashReportURL); err != nil {
		panic(errors.Wrap(err, "failed to setup crash reporting"))
	}

	if cmd == "start" {
		cmd = "server"
	}
	info := build.GetInfo()
	raven.SetRelease(info.Tag)
	raven.SetEnvironment(info.Type)
	raven.SetTagsContext(map[string]string{
		"cmd":          cmd,
		"platform":     info.Platform,
		"distribution": info.Distribution,
		"rev":          info.Revision,
		"goversion":    info.GoVersion,
	})
}

var crdbPaths = []string{
	"github.com/cockroachdb/cockroach",
	"github.com/coreos/etcd/raft",
}

func uptimeTag(now time.Time) string {
	uptime := now.Sub(startTime)
	switch {
	case uptime < 1*time.Second:
		return "<1s"
	case uptime < 10*time.Second:
		return "<10s"
	case uptime < 1*time.Minute:
		return "<1m"
	case uptime < 10*time.Minute:
		return "<10m"
	case uptime < 1*time.Hour:
		return "<1h"
	case uptime < 10*time.Hour:
		return "<10h"
	default:
		daysUp := int(uptime / (24 * time.Hour))
		return fmt.Sprintf("<%dd", daysUp+1)
	}
}

func sendCrashReport(ctx context.Context, sv *settings.Values, r interface{}, depth int) {
	if !DiagnosticsReportingEnabled.Get(sv) || !CrashReports.Get(sv) {
		return // disabled via settings.
	}

	if raven.DefaultClient == nil {
		return // disabled via empty URL env var.
	}

	var err error
	if e, ok := r.(error); ok {
		err = e
	} else {
		err = fmt.Errorf("%v", r)
	}

	// This is close to inlining raven.CaptureErrorAndWait(), except it lets us
	// control the stack depth of the collected trace.
	const contextLines = 3

	ex := raven.NewException(err, raven.NewStacktrace(depth+1, contextLines, crdbPaths))
	packet := raven.NewPacket(err.Error(), ex)
	// Avoid leaking the machine's hostname by injecting the literal "<redacted>".
	// Otherwise, raven.Client.Capture will see an empty ServerName field and
	// automatically fill in the machine's hostname.
	packet.ServerName = "<redacted>"
	tags := map[string]string{
		"uptime": uptimeTag(time.Now()),
	}
	eventID, ch := raven.DefaultClient.Capture(packet, tags)
	select {
	case <-ch:
		Shout(ctx, Severity_ERROR, "Reported as error "+eventID)
	case <-time.After(10 * time.Second):
		Shout(ctx, Severity_ERROR, "Time out trying to submit crash report")
	}
}
