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

// readEmitPayload 读 --data-file（`-` = stdin）；体积判据只在网关与中枢（那里有下游 schema）。
func readEmitPayload(name string) ([]byte, error) {
	if name == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(name)
}
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
	data := fs.String("data", "", "JSON object")
	// --data-file：argv 有 ARG_MAX（单个参数 ~128 KiB 就 E2BIG），大 payload 只能从文件进（#571）。
	dataFile := fs.String("data-file", "", "read the JSON object from a file, or - for stdin")
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
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if positional != "" {
		if *to != "" && *to != positional {
			return emitFailure(out, errOut, protocol.ReasonMissingTarget, "conflicting emit targets", 2)
		}
		*to = positional
	}
	if strings.TrimSpace(*to) == "" {
		return emitFailure(out, errOut, protocol.ReasonMissingTarget, "emit target is required", 2)
	}
	payload := "{}"
	switch {
	case given["data"] && given["data-file"]:
		return emitFailure(out, errOut, protocol.ReasonParamType, "--data 与 --data-file 只能给一个", 2)
	case given["data"]:
		payload = *data
	case given["data-file"]:
		raw, readErr := readEmitPayload(*dataFile)
		if readErr != nil {
			return emitFailure(out, errOut, protocol.ReasonParamType, "--data-file: "+readErr.Error(), 2)
		}
		payload = string(raw)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &obj); err != nil || obj == nil {
		return emitFailure(out, errOut, protocol.ReasonParamType, "--data must be a JSON object", 2)
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
	raw, _ := json.Marshal(protocol.EmitRequest{To: *to, ID: *id, Data: json.RawMessage(payload)})
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
	// 退出码只看**结论码**（413 params_too_large 漏成 5 就是 #571）；message 只拼结论码，不回显服务端文本。
	if !protocol.IsEmitErrorCode(failure.Error) {
		return emitFailure(out, errOut, protocol.ReasonEmitUnreachable, "emit failed; result unknown", 5)
	}
	message := fmt.Sprintf("emit failed: %s (HTTP %d)", failure.Error, resp.StatusCode)
	return emitFailure(out, errOut, failure.Error, message, protocol.EmitExitCode(failure.Error))
}
