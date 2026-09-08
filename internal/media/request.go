package media

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type mediaRequest struct {
	URL       string
	Display   string
	Referer   string
	Origin    string
	UserAgent string
	Cookie    string
	HeaderArg []string
	CookieArg []string
	Secrets   []string
}

// normalizeRequest preserves the package-local helper's original signature
// for callers that do not need an Origin header.
func normalizeRequest(rawURL, referer, userAgent, cookie string) (mediaRequest, error) {
	return normalizeRequestWithOrigin(rawURL, referer, "", userAgent, cookie)
}

func normalizeRequestWithOrigin(rawURL, referer, origin, userAgent, cookie string) (mediaRequest, error) {
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := parseMediaURL(rawURL)
	if err != nil {
		return mediaRequest{}, err
	}
	origin, err = normalizeOriginHeader(origin)
	if err != nil {
		return mediaRequest{}, err
	}
	for _, header := range []struct {
		name  string
		value string
	}{
		{name: "Referer", value: referer},
		{name: "Origin", value: origin},
		{name: "User-Agent", value: userAgent},
		{name: "Cookie", value: cookie},
	} {
		if err := validateHeaderValue(header.name, header.value); err != nil {
			return mediaRequest{}, err
		}
	}
	headerArg, err := makeHeaderArgWithOrigin(referer, origin, userAgent, cookie)
	if err != nil {
		return mediaRequest{}, err
	}
	cookieArg, err := makeScopedCookieArg(parsed, cookie)
	if err != nil {
		return mediaRequest{}, err
	}
	secrets := make([]string, 0, 4)
	for _, value := range []string{referer, origin, userAgent, cookie} {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	return mediaRequest{
		URL: rawURL, Display: displayURL(parsed), Referer: referer, Origin: origin, UserAgent: userAgent,
		Cookie: cookie, HeaderArg: headerArg, CookieArg: cookieArg, Secrets: secrets,
	}, nil
}

func normalizeOriginHeader(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "null" {
		return value, nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("Origin header must be an HTTP origin")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func parseMediaURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: url is required", ErrInvalidURL)
	}
	if strings.ContainsAny(raw, "\r\n\x00") {
		return nil, fmt.Errorf("%w: url contains invalid control characters", ErrInvalidURL)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, ErrInvalidURL
	}
	return parsed, nil
}

func displayURL(parsed *url.URL) string {
	clone := *parsed
	clone.User = nil
	clone.RawQuery = ""
	clone.ForceQuery = false
	clone.Fragment = ""
	return clone.String()
}

func validateHeaderValue(name, value string) error {
	if len(value) > maxHeaderValue {
		return fmt.Errorf("%s header is too long", name)
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s header contains invalid control characters", name)
	}
	return nil
}

func makeHeaderArg(referer, userAgent, cookie string) ([]string, error) {
	return makeHeaderArgWithOrigin(referer, "", userAgent, cookie)
}

func makeHeaderArgWithOrigin(referer, origin, userAgent, cookie string) ([]string, error) {
	for _, header := range []struct {
		name  string
		value string
	}{
		{name: "Referer", value: referer},
		{name: "Origin", value: origin},
		{name: "User-Agent", value: userAgent},
		{name: "Cookie", value: cookie},
	} {
		if err := validateHeaderValue(header.name, header.value); err != nil {
			return nil, err
		}
	}
	headers := make([]string, 0, 4)
	for _, header := range []struct {
		name  string
		value string
	}{
		{name: "Referer", value: referer},
		{name: "Origin", value: origin},
		{name: "User-Agent", value: userAgent},
	} {
		if header.value != "" {
			headers = append(headers, header.name+": "+header.value)
		}
	}
	if len(headers) == 0 {
		return nil, nil
	}
	// FFmpeg's HTTP protocol expects a CRLF-delimited header block. Values have
	// already been checked for CR/LF, so a caller cannot inject another header.
	return []string{"-headers", strings.Join(headers, "\r\n") + "\r\n"}, nil
}

func makeScopedCookieArg(source *url.URL, cookie string) ([]string, error) {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return nil, nil
	}
	if source == nil || source.Hostname() == "" {
		return nil, ErrInvalidURL
	}
	lines := make([]string, 0, 4)
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" || strings.ContainsAny(name, " \t\r\n") {
			return nil, errors.New("Cookie header contains an invalid cookie")
		}
		value = strings.TrimSpace(value)
		lines = append(lines, name+"="+value+"; path=/; domain="+source.Hostname()+";")
	}
	if len(lines) == 0 {
		return nil, errors.New("Cookie header contains no cookies")
	}
	return []string{"-cookies", strings.Join(lines, "\n")}, nil
}

