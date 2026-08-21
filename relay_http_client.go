package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func relayProfileUsesHTTPProxy(profile relayProfile) bool {
	if profile.RelayMode != "mixedApi" && profile.RelayMode != "pureApi" {
		return false
	}
	return profile.ProxyEnabled && strings.TrimSpace(profile.ProxyURL) != ""
}

func relayProfileProxyURL(profile relayProfile) (*url.URL, error) {
	if !profile.ProxyEnabled {
		return nil, nil
	}
	rawURL := strings.TrimSpace(profile.ProxyURL)
	if rawURL == "" {
		return nil, nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("HTTP 代理地址无效：%s", rawURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("HTTP 代理地址无效：仅支持 http:// 或 https:// 代理 URL")
	}
	return parsed, nil
}

func relayHTTPClient(profile relayProfile) (*http.Client, error) {
	proxyURL, err := relayProfileProxyURL(profile)
	if err != nil {
		return nil, err
	}
	if proxyURL == nil {
		return http.DefaultClient, nil
	}
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		return &http.Client{Transport: transport}, nil
	}
	transport := baseTransport.Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	return &http.Client{Transport: transport}, nil
}

func relayHTTPClientForSettings(settings backendSettings, profile relayProfile, purpose proxyPurpose) (*http.Client, error) {
	return proxyHTTPClient(settings, profile, purpose)
}
