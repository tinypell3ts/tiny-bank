package kit

import (
	"fmt"
	"net/url"
)

// ServiceURL reads setting, the base URL of another service (like
// ACCOUNT_URL=http://account), from getenv. It must be an http(s) URL with a
// host and nothing after it, and comes back without a trailing slash, ready
// for paths to be added ("http://account" + "/accounts/…"). A missing or
// malformed one is an error naming the setting, so a typo stops the service
// starting instead of failing every call.
func ServiceURL(getenv func(string) string, setting string) (string, error) {
	raw := getenv(setting)
	if raw == "" {
		return "", fmt.Errorf("%s is required", setting)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return "", fmt.Errorf("%s must be an http(s) URL with just a host, like http://account", setting)
	}
	return u.Scheme + "://" + u.Host, nil
}
