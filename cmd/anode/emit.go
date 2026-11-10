package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

func trimToken(s string) string { return strings.TrimSpace(s) }
func emit(args []string, out, errOut io.Writer) int {
	return emitWithTimeout(args, out, errOut, 30*time.Second)
}
func emitFailure(out, errOut io.Writer, reason, message string, code int) int {
	_ = json.NewEncoder(out).Encode(protocol.EmitError{Error: reason, Message: message})
	fmt.Fprintln(errOut, strings.ReplaceAll(strings.ReplaceAll(message, "\n", " "), "\r", " "))
	return code
}
func emitWithTimeout(args []string, out, errOut io.Writer, timeout time.Duration) int {
	fs := flag.NewFlagSet("emit", flag.ContinueOnError)
	var parseLog bytes.Buffer
	fs.SetOutput(&parseLog)
	to := fs.String("to", "", "target declared in spec.next")
	id := fs.String("id", "", "optional logical idempotency key")
	data := fs.String("data", "{}", "JSON object")
	// flag supports a positional target before flags without adding another parser/alias.
	positional := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, parseLog.String())
			return 0
		}
		return emitFailure(out, errOut, protocol.ReasonMissingTarget, "invalid emit flags", 2)
	}
	if fs.NArg() == 1 && positional == "" {
		positional = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return emitFailure(out, errOut, protocol.ReasonMissingTarget, "expected one emit target", 2)
	}
	if positional != "" {
		if *to != "" && *to != positional {
			return emitFailure(out, errOut, protocol.ReasonMissingTarget, "conflicting emit targets", 2)
		}
		*to = positional
	}
	if strings.TrimSpace(*to) == "" {
		return emitFailure(out, errOut, protocol.ReasonMissingTarget, "emit target is required", 2)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*data), &obj); err != nil || obj == nil {
		return emitFailure(out, errOut, protocol.ReasonTargetNotDeclared, "--data must be a JSON object", 2)
	}
	token := os.Getenv(protocol.EnvTaskToken)
	if token == "" {
		return emitFailure(out, errOut, protocol.ReasonTokenInvalid, "task token is missing or invalid", 3)
	}
	endpoint := os.Getenv(protocol.EnvEmitEndpoint)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	endpointURL, err := url.Parse(endpoint)
	if err != nil || endpointURL == nil {
		return emitFailure(out, errOut, protocol.ReasonEmitUnreachable, "emit endpoint is invalid; result unknown", 5)
	}
	address := ""
	switch endpointURL.Scheme {
	case "unix":
		if endpointURL.Host != "" || endpointURL.Path == "" || endpointURL.RawQuery != "" || endpointURL.Fragment != "" {
			return emitFailure(out, errOut, protocol.ReasonEmitUnreachable, "invalid Unix emit endpoint; result unknown", 5)
		}
		socket := endpointURL.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", socket)
		}
		address = "http://unix" + protocol.EmitPath
	case "http", "https":
		if endpointURL.Host == "" || endpointURL.User != nil || endpointURL.RawQuery != "" || endpointURL.Fragment != "" {
			return emitFailure(out, errOut, protocol.ReasonEmitUnreachable, "invalid emit endpoint; result unknown", 5)
		}
		endpointURL.Path = strings.TrimRight(endpointURL.Path, "/") + protocol.EmitPath
		address = endpointURL.String()
	default:
		return emitFailure(out, errOut, protocol.ReasonEmitUnreachable, "emit endpoint unavailable; result unknown", 5)
	}
	raw, _ := json.Marshal(protocol.EmitRequest{To: *to, ID: *id, Data: json.RawMessage(*data)})
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(raw))
	if err != nil {
		return emitFailure(out, errOut, protocol.ReasonEmitUnreachable, "invalid request; result unknown", 5)
	}
	req.Header.Set(protocol.EmitAuthHeader, protocol.EmitAuthScheme+" "+token)
	req.Header.Set("Content-Type", protocol.EmitContentType)
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return emitFailure(out, errOut, protocol.ReasonEmitUnreachable, "emit network failure; result unknown (do not replay whole task)", 5)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		return emitFailure(out, errOut, protocol.ReasonEmitUnreachable, "invalid emit response; result unknown", 5)
	}
	if resp.StatusCode == http.StatusOK {
		var success protocol.EmitResponse
		if err := json.Unmarshal(body, &success); err != nil || success.To == "" || (success.TaskID == nil && success.Pruned == nil) {
			return emitFailure(out, errOut, protocol.ReasonEmitUnreachable, "invalid success receipt; result unknown", 5)
		}
		_ = json.NewEncoder(out).Encode(success)
		return 0
	}
	var failure protocol.EmitError
	if err := json.Unmarshal(body, &failure); err != nil {
		failure.Error = protocol.ReasonEmitUnreachable
	}
	code := 5
	switch resp.StatusCode {
	case 400:
		code = 2
	case 401:
		code = 3
	case 409:
		code = protocol.EmitExitCode(failure.Error)
	}
	// Do not expose arbitrary response messages (a server may reflect credentials).
	message := fmt.Sprintf("emit failed: %s (HTTP %d)", failure.Error, resp.StatusCode)
	if !protocol.IsEmitErrorCode(failure.Error) {
		failure.Error = protocol.ReasonEmitUnreachable
		message = "emit failed; result unknown"
	}
	return emitFailure(out, errOut, failure.Error, message, code)
}
