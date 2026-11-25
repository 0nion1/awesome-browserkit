package browserkit

import (
	"errors"
	"net/url"
	"strings"

	"github.com/go-rod/rod/lib/proto"
)

type browserProxy struct {
	server        string
	username      string
	password      string
	authenticated bool
}

func parseBrowserProxy(value string) (browserProxy, error) {
	proxy := browserProxy{server: strings.TrimSpace(value)}
	if !strings.Contains(proxy.server, "@") {
		return proxy, nil
	}
	parsed, err := url.Parse(proxy.server)
	if err != nil || parsed.User == nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" {
		return browserProxy{}, errors.New("认证代理需要 http://user:password@host:port 或 https://user:password@host:port")
	}
	proxy.username = parsed.User.Username()
	proxy.password, _ = parsed.User.Password()
	proxy.authenticated = true
	parsed.User = nil
	parsed.Path = ""
	proxy.server = parsed.String()
	return proxy, nil
}

func (proxy browserProxy) challengeResponse(challenge *proto.FetchAuthChallenge, retried bool) *proto.FetchAuthChallengeResponse {
	response := &proto.FetchAuthChallengeResponse{Response: proto.FetchAuthChallengeResponseResponseCancelAuth}
	if !proxy.authenticated || retried || challenge == nil || challenge.Source != proto.FetchAuthChallengeSourceProxy {
		return response
	}
	origin, err := url.Parse(challenge.Origin)
	server, serverErr := url.Parse(proxy.server)
	if err != nil || serverErr != nil || proxyEndpoint(origin) != proxyEndpoint(server) {
		return response
	}
	response.Response = proto.FetchAuthChallengeResponseResponseProvideCredentials
	response.Username, response.Password = proxy.username, proxy.password
	return response
}

func proxyEndpoint(address *url.URL) string {
	port := address.Port()
	if port == "" {
		if address.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(address.Hostname()) + ":" + port
}
