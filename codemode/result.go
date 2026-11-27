package codemode

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dailz1/go-agent/tool"
)

// outputMarkerReserve is kept out of the display budget so omission markers
// always fit.
const outputMarkerReserve = 256

const (
	resultOmittedMarker   = "[codemode: result omitted: output limit]"
	stdoutTruncatedMarker = "[codemode: stdout truncated]"
	imageOmittedMarker    = "[codemode: image omitted]"
)

// assembleResponse folds the selected cause, the executor outcome and the
// cleanup facts into the outer ToolResult. Supervisor-selected
// causes and terminal script failures return soft errors with explicit
// cleanup state; a hard fault with no prior cause never reaches here —
// supervise returned it as a wrapped Go error.
func (e *execution) assembleResponse() execResponse {
	e.mu.Lock()
	causeSet, cause, causeMsg := e.causeSet, e.cause, e.causeMsg
	vmExited, host, dif := e.vmExited, e.hostOutstanding, e.deliveriesInFlight
	outcome := e.outcome
	e.mu.Unlock()

	cleanupIncomplete := !vmExited || host > 0 || dif > 0
	cleanup := fmt.Sprintf(" (cleanup: vmExited=%v hostOutstanding=%d deliveriesInFlight=%d)", vmExited, host, dif)

	if causeSet {
		switch cause {
		case FailureScript, FailureEncoding:
			return execResponse{
				result:            tool.NewErrorResult("code execution failed: %s", causeMsg),
				cleanupIncomplete: cleanupIncomplete,
			}
		default:
			if cause == FailureInternal && e.hardPending.Load() {
				// A registered hard fault whose buffered value was not yet
				// observed still propagates as the Go error.
				return execResponse{hardErr: &hardAgentError{category: causeMsg}}
			}
			msg := fmt.Sprintf("code execution stopped: %s: %s", cause, causeMsg)
			if cleanupIncomplete {
				msg += cleanup
			}
			return execResponse{
				result:            tool.NewErrorResult("%s", msg),
				cleanupIncomplete: cleanupIncomplete,
			}
		}
	}
	return execResponse{result: projectSuccess(e, outcome), cleanupIncomplete: cleanupIncomplete}
}

// projectSuccess assembles the model-visible result of a successful run:
// terminal value first, then explicitly selected images in script order,
// then captured stdout — all within OutputBytes minus the marker reserve.
// The outer Data channel stays nil: the model's structured payload is the
// script's own return value in Content.
func projectSuccess(e *execution, outcome *RunOutcome) *tool.ToolResult {
	if outcome == nil {
		return tool.NewErrorResult("code execution produced no outcome")
	}
	if outcome.Failure != nil {
		return tool.NewErrorResult("code execution failed: %s", outcome.Failure.Message)
	}
	budget := e.limits.OutputBytes - outputMarkerReserve

	content := resultOmittedMarker
	if !outcome.ValueOmitted && outcome.Value != nil {
		raw := string(outcome.Value)
		if raw == "null" {
			content = "null"
		} else {
			var s string
			if err := json.Unmarshal(outcome.Value, &s); err == nil {
				content = s
			} else {
				content = raw
			}
		}
	}
	spent := len(content)

	var imgs []tool.Image
	omitted := 0
	for _, ref := range outcome.Images {
		if !strings.HasPrefix(ref.Token, e.id+"-") {
			// Cross-execution or forged refs fail closed; the token is never
			// echoed verbatim into the response.
			return tool.NewErrorResult("code execution failed: image ref is unknown for this execution")
		}
		img, ok := e.lookupImage(ref.Token)
		if !ok {
			return tool.NewErrorResult("code execution failed: image ref is unknown for this execution")
		}
		cost := len(img.Data) + len(img.URL) + len(img.MIMEType) + 128
		if spent+cost > budget {
			omitted++ // repeated refs are allowed; the marker is capped below
			continue
		}
		spent += cost
		imgs = append(imgs, img)
	}
	if omitted > 0 {
		marker := fmt.Sprintf("[codemode: image omitted x%d]", omitted)
		if spent+1+len(marker) <= e.limits.OutputBytes {
			content += "\n" + marker
			spent += 1 + len(marker)
		}
	}

	if stdout := e.stdoutSnapshot(); len(stdout) > 0 && spent <= budget {
		remaining := budget - spent
		chunk := string(stdout)
		truncated := e.stdoutTruncated()
		if len(chunk) > remaining {
			chunk = cutAtRuneBoundary(chunk, remaining)
			truncated = true
		}
		truncMarker := "\n" + stdoutTruncatedMarker
		if len(chunk)+len(truncMarker) <= e.limits.OutputBytes-spent {
			content += "\n" + chunk
			spent += len(chunk)
			if truncated && spent+len(truncMarker) <= e.limits.OutputBytes {
				content += truncMarker
			}
		} else if spent+len(truncMarker) <= e.limits.OutputBytes {
			// No room for even a prefix: still signal the suppressed stdout
			// with the marker.
			content += truncMarker
		}
	}
	return &tool.ToolResult{Content: content, Images: imgs}
}

func (e *execution) stdoutSnapshot() []byte {
	e.stdoutMu.Lock()
	defer e.stdoutMu.Unlock()
	return append([]byte(nil), e.stdoutBuf...)
}

func (e *execution) stdoutTruncated() bool {
	e.stdoutMu.Lock()
	defer e.stdoutMu.Unlock()
	return e.stdoutFull
}
