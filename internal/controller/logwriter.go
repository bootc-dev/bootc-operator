// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strings"

	"github.com/go-logr/logr"
)

// logWriter adapts a logr.Logger to io.Writer. drain.Helper uses
// io.Writer (Out and ErrOut) for its output rather than a structured
// logger. This adapter bridges the two so drain progress and errors
// flow through our logr pipeline with appropriate levels.
type logWriter struct {
	prefix  string
	logFunc func(msg string, keysAndValues ...any)
}

func (w logWriter) Write(p []byte) (int, error) {
	w.logFunc(w.prefix + strings.TrimSpace(string(p)))
	return len(p), nil
}

// newDrainOutWriter returns an io.Writer that logs drain stdout at
// V(1) level, prefixed with "Draining <nodeName>: stdout: ".
func newDrainOutWriter(log logr.Logger, nodeName string) logWriter {
	return logWriter{
		prefix:  fmt.Sprintf("Draining %s: stdout: ", nodeName),
		logFunc: log.V(1).Info,
	}
}

// newDrainErrWriter returns an io.Writer that logs drain stderr at
// V(0) (Info) level, prefixed with "Draining <nodeName>: stderr: ".
func newDrainErrWriter(log logr.Logger, nodeName string) logWriter {
	return logWriter{
		prefix:  fmt.Sprintf("Draining %s: stderr: ", nodeName),
		logFunc: log.Info,
	}
}
