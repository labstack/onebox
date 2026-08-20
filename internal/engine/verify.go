package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/labstack/onebox/internal/app"
)

const maxVerificationBodyBytes = 1 << 20

// verifyURL is the runner-side edge check (ob.sh's smoke test, absorbed).
func (e *Engine) verifyURL(ctx context.Context, chk app.RunnableCheck) error {
	label := verificationURLLabel(chk.URL)
	client := &http.Client{
		Timeout: e.Opts.HTTPTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chk.URL, nil)
	if err != nil {
		return fmt.Errorf("verify %s: invalid URL", label)
	}
	resp, err := client.Do(req)
	if err != nil {
		err = verificationRequestErrorDetail(err)
		return fmt.Errorf("verify %s: request failed: %v", label, err)
	}
	defer resp.Body.Close()
	if !verificationStatusAllowed(resp.StatusCode, chk.StatusCodes) {
		return fmt.Errorf("verify %s: unexpected status %d", label, resp.StatusCode)
	}
	if err := verifyResponseHeaders(resp.Header, chk.RequiredHeaders); err != nil {
		return fmt.Errorf("verify %s: %w", label, err)
	}
	if chk.Contains == "" && len(chk.JSONAssertions) == 0 {
		return nil
	}
	body, err := readVerificationBody(resp.Body)
	if err != nil {
		return fmt.Errorf("verify %s: %w", label, err)
	}
	if chk.Contains != "" && !strings.Contains(string(body), chk.Contains) {
		return fmt.Errorf("verify %s: response body is missing the configured substring", label)
	}
	if err := verifyJSONAssertions(body, chk.JSONAssertions); err != nil {
		return fmt.Errorf("verify %s: %w", label, err)
	}
	return nil
}

func verificationRequestErrorDetail(err error) error {
	for {
		urlErr, ok := err.(*url.Error)
		if !ok {
			return err
		}
		err = urlErr.Err
	}
}

func verificationURLLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<configured URL>"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

func verificationStatusAllowed(status int, allowed []int) bool {
	if len(allowed) == 0 {
		return status >= http.StatusOK && status < http.StatusMultipleChoices
	}
	for _, candidate := range allowed {
		if status == candidate {
			return true
		}
	}
	return false
}

func verifyResponseHeaders(headers http.Header, required map[string]string) error {
	names := make([]string, 0, len(required))
	for name := range required {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		values, found := responseHeaderValues(headers, name)
		if !found {
			return fmt.Errorf("required response header %q is missing", name)
		}
		matched := false
		for _, value := range values {
			if value == required[name] {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("required response header %q has an unexpected value", name)
		}
	}
	return nil
}

func responseHeaderValues(headers http.Header, wanted string) ([]string, bool) {
	for name, values := range headers {
		if strings.EqualFold(name, wanted) {
			return values, true
		}
	}
	return nil, false
}

func readVerificationBody(body io.Reader) ([]byte, error) {
	limited, err := io.ReadAll(io.LimitReader(body, maxVerificationBodyBytes+1))
	if err != nil {
		return nil, errors.New("could not read response body")
	}
	if len(limited) > maxVerificationBodyBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxVerificationBodyBytes)
	}
	return limited, nil
}

func verifyJSONAssertions(body []byte, assertions []app.JSONAssertion) error {
	if len(assertions) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return errors.New("response body is not valid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("response body is not a single JSON value")
	}
	for _, assertion := range assertions {
		value, found := jsonValueAtPath(document, assertion.Path)
		if !found {
			return fmt.Errorf("JSON path %q was not found", assertion.Path)
		}
		if !jsonScalarsEqual(value, assertion.Equals) {
			return fmt.Errorf("JSON assertion at path %q did not match", assertion.Path)
		}
	}
	return nil
}

func jsonValueAtPath(document any, path string) (any, bool) {
	value := document
	for _, segment := range strings.Split(path, ".") {
		switch current := value.(type) {
		case map[string]any:
			var found bool
			value, found = current[segment]
			if !found {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(current) {
				return nil, false
			}
			value = current[index]
		default:
			return nil, false
		}
	}
	return value, true
}

func jsonScalarsEqual(actual, expected any) bool {
	if expected == nil {
		return actual == nil
	}
	if actual == nil {
		return false
	}
	actualNumber, actualIsNumber := scalarNumberString(actual)
	expectedNumber, expectedIsNumber := scalarNumberString(expected)
	if actualIsNumber || expectedIsNumber {
		if !actualIsNumber || !expectedIsNumber {
			return false
		}
		actualRat, actualOK := new(big.Rat).SetString(actualNumber)
		expectedRat, expectedOK := new(big.Rat).SetString(expectedNumber)
		return actualOK && expectedOK && actualRat.Cmp(expectedRat) == 0
	}
	actualValue, expectedValue := reflect.ValueOf(actual), reflect.ValueOf(expected)
	if actualValue.Kind() != expectedValue.Kind() {
		return false
	}
	switch actualValue.Kind() {
	case reflect.String:
		return actualValue.String() == expectedValue.String()
	case reflect.Bool:
		return actualValue.Bool() == expectedValue.Bool()
	default:
		return false
	}
}

func scalarNumberString(value any) (string, bool) {
	if number, ok := value.(json.Number); ok {
		return number.String(), true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10), true
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, v.Type().Bits()), true
	default:
		return "", false
	}
}

// Verify runs host-side checks against the container network — never through
// the edge, because an edge blip must not fail a healthy release. URL
// checks go through the edge from the runner and are advisory territory.
func (e *Engine) Verify(ctx context.Context) error {
	for _, chk := range e.Spec.Checks.All() {
		if chk.MigrationRevisions != nil {
			assertion := chk.MigrationRevisions
			result, ok := e.jobResults[assertion.Job]
			if !ok {
				return fmt.Errorf("verify migration revisions for %s: provider job-result evidence is unavailable", assertion.Job)
			}
			if result.Provider != assertion.Provider {
				return fmt.Errorf("verify migration revisions for %s: recorded provider does not match", assertion.Job)
			}
			if !equalRevisionLists(result.AfterRevisions, assertion.AppliedRevisions) {
				return fmt.Errorf("verify migration revisions for %s: applied revisions do not match the bound expectation", assertion.Job)
			}
			e.logf("verify migration revisions %s/%s: ok", assertion.Provider, assertion.Job)
			continue
		}
		if chk.URL != "" {
			if err := e.verifyURL(ctx, chk); err != nil {
				if chk.Advisory {
					e.logf("warn (advisory): %v", err)
					continue
				}
				return err
			}
			e.logf("verify %s: ok", verificationURLLabel(chk.URL))
			continue
		}
		role, ok := e.Spec.Workloads[chk.Workload]
		if !ok {
			return fmt.Errorf("verify: unknown workload %q", chk.Workload)
		}
		id, err := e.containerID(ctx, chk.Workload)
		if err != nil {
			return err
		}
		if id == "" {
			return fmt.Errorf("verify %s: no running container", chk.Workload)
		}
		switch {
		case chk.HTTP != "":
			port := chk.Port
			if port == 0 && role.Health != nil {
				port = role.Health.Port
			}
			if port == 0 {
				return fmt.Errorf("verify %s: no port (set the check's port, or the workload's health port)", chk.Workload)
			}
			res, err := e.T.Run(ctx, "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "+id)
			if err != nil {
				return err
			}
			fields := strings.Fields(res.Stdout)
			if len(fields) == 0 {
				return fmt.Errorf("verify %s: container %s has no network address", chk.Workload, id)
			}
			ip := fields[0]
			if strings.ContainsAny(ip, ";|&$`'\"") {
				return fmt.Errorf("verify %s: suspicious address %q", chk.Workload, ip)
			}
			// The whole URL goes in as one quoted argument, because the path is
			// authored. The project grammar for a URL path refuses quotes, `$`,
			// backticks and spaces, but permits `;`, `>` and `|` — enough for
			// `/healthz;id>/tmp/x` to end the probe and run a second command as
			// whoever deploys.
			//
			// That covers the address too, which makes the check above redundant
			// against injection. It stays anyway: a container address holding a
			// shell metacharacter means the inspect returned something this code
			// does not understand, and refusing says so where quoting would carry
			// on and fail later as a confusing curl error.
			cres, err := e.T.Run(ctx, "curl -fsS -m 5 "+q(fmt.Sprintf("http://%s:%d%s", ip, port, chk.HTTP)))
			if err != nil {
				return err
			}
			if cres.ExitCode != 0 {
				return fmt.Errorf("verify %s: GET %s -> exit %d %s", chk.Workload, chk.HTTP, cres.ExitCode, strings.TrimSpace(cres.Stderr))
			}
		case chk.Exec != "":
			res, err := e.T.Run(ctx, "docker exec "+id+" sh -c "+q(chk.Exec))
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("verify %s: exec failed (%d): %s", chk.Workload, res.ExitCode, strings.TrimSpace(res.Stderr))
			}
		}
		e.logf("verify %s: ok", chk.Workload)
	}
	return nil
}
