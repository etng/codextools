package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImageRelayUsesImageEndpointKeyAndHTTPProxyOnlyForImageRequests(t *testing.T) {
	type upstreamRequest struct {
		path          string
		authorization string
		body          string
	}
	requests := make([]upstreamRequest, 0, 2)
	proxyHits := 0
	upstream, proxy := newForwardingProxyPair(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		requests = append(requests, upstreamRequest{
			path:          req.URL.Path,
			authorization: req.Header.Get("Authorization"),
			body:          string(body),
		})
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	}), func() { proxyHits++ })
	defer upstream.Close()
	defer proxy.Close()

	profile := relayProfile{
		ID:                            "relay",
		Name:                          "Relay",
		RelayMode:                     "pureApi",
		Protocol:                      "responses",
		BaseURL:                       upstream.URL + "/text/v1",
		APIKey:                        "text-key",
		ImageGenerationEnabled:        true,
		ImageGenerationUseSeparateAPI: true,
		ImageGenerationBaseURL:        upstream.URL + "/image/v1/images/generations",
		ImageGenerationAPIKey:         "image-key",
		ProxyEnabled:                  true,
		ProxyURL:                      proxy.URL,
	}

	imageBody := []byte(`{"model":"gpt-image-1","prompt":"draw a lighthouse"}`)
	imageReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:57323/v1/images/generations", strings.NewReader(string(imageBody)))
	imageRec := httptest.NewRecorder()
	if !forwardRelayProxyAttempt(defaultSettings(), imageRec, imageReq, imageBody, profile, 1, 1) {
		t.Fatal("image relay attempt did not complete")
	}
	if imageRec.Code != http.StatusOK {
		t.Fatalf("image relay returned %d: %s", imageRec.Code, imageRec.Body.String())
	}

	textBody := []byte(`{"model":"gpt-test","input":"修复图片生成路由，只在生成图片的时候调用独立 API","tools":[{"type":"image_generation"},{"type":"web_search"}]}`)
	textReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:57323/v1/responses", strings.NewReader(string(textBody)))
	textRec := httptest.NewRecorder()
	if !forwardRelayProxyAttempt(defaultSettings(), textRec, textReq, textBody, profile, 1, 1) {
		t.Fatal("text relay attempt did not complete")
	}
	if textRec.Code != http.StatusOK {
		t.Fatalf("text relay returned %d: %s", textRec.Code, textRec.Body.String())
	}

	if proxyHits != 2 {
		t.Fatalf("both upstream calls should use the configured HTTP proxy, got %d hits", proxyHits)
	}
	if len(requests) != 2 {
		t.Fatalf("expected two upstream requests, got %#v", requests)
	}
	if got, want := requests[0].path, "/image/v1/images/generations"; got != want {
		t.Fatalf("image endpoint was duplicated or changed: got %q want %q", got, want)
	}
	if got, want := requests[0].authorization, "Bearer image-key"; got != want {
		t.Fatalf("image request used the wrong key: got %q want %q", got, want)
	}
	if got, want := requests[1].path, "/text/v1/responses"; got != want {
		t.Fatalf("text request used the wrong endpoint: got %q want %q", got, want)
	}
	if got, want := requests[1].authorization, "Bearer text-key"; got != want {
		t.Fatalf("text request used the wrong key: got %q want %q", got, want)
	}
	if strings.Contains(requests[1].body, `"type":"image_generation"`) {
		t.Fatalf("text request still exposed the image tool upstream: %s", requests[1].body)
	}
}

func TestRelayRouteReasonOnlyReportsImageDisabledWhenToolWasRemoved(t *testing.T) {
	profile := relayProfile{
		Protocol:               "responses",
		ImageGenerationEnabled: false,
	}

	empty := decideRelayRouteForPath("/v1/models", nil, profile)
	if empty.reason != "empty_body" {
		t.Fatalf("an ordinary bodyless request should not be reported as invalid JSON: %#v", empty)
	}

	textBody := []byte(`{"model":"gpt-test","input":"implement a parser"}`)
	text := decideRelayRouteForPath("/v1/responses", textBody, profile)
	if text.reason != "default_text" || text.strippedImageTool {
		t.Fatalf("an ordinary text request should not be reported as image-disabled routing: %#v", text)
	}

	imageToolBody := []byte(`{"model":"gpt-test","input":"implement a parser","tools":[{"type":"image_generation"},{"type":"web_search"}]}`)
	stripped := decideRelayRouteForPath("/v1/responses", imageToolBody, profile)
	if stripped.reason != "image_disabled" || !stripped.strippedImageTool {
		t.Fatalf("image-disabled routing should be reported only when the tool was removed: %#v", stripped)
	}
}

func TestImageRelayTargetAcceptsBaseAndCompleteEndpoints(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		path    string
		want    string
	}{
		{name: "host", baseURL: "https://images.example.test/v1", path: "/v1/images/generations", want: "https://images.example.test/v1/images/generations"},
		{name: "responses endpoint", baseURL: "https://images.example.test/v1/responses", path: "/v1/responses", want: "https://images.example.test/v1/responses"},
		{name: "images endpoint", baseURL: "https://images.example.test/v1/images/generations", path: "/v1/images/generations", want: "https://images.example.test/v1/images/generations"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := relayImageTargetURL(test.baseURL, test.path); got != test.want {
				t.Fatalf("image target mismatch: got %q want %q", got, test.want)
			}
		})
	}
}

func TestImageRelayRouteRequiresImageToolForTextIntent(t *testing.T) {
	profile := relayProfile{
		ImageGenerationEnabled:        true,
		ImageGenerationUseSeparateAPI: true,
		ImageGenerationBaseURL:        "https://images.example.test/v1",
		ImageGenerationAPIKey:         "image-key",
	}

	withoutTool := decideRelayRoute([]byte(`{"input":"帮我生成一张图片"}`), profile)
	if withoutTool.useImageAPI {
		t.Fatal("text intent without an image tool must stay on the default relay")
	}
	withTool := decideRelayRoute([]byte(`{"input":"帮我生成一张图片","tools":[{"type":"image_generation"}]}`), profile)
	if !withTool.useImageAPI {
		t.Fatal("explicit image intent with an image tool should use the image relay")
	}
	technical := decideRelayRoute([]byte(`{"input":"恢复图片生成 API，并且只在生成图片的时候调用","tools":[{"type":"image_generation"}]}`), profile)
	if technical.useImageAPI {
		t.Fatal("technical discussion about image routing must stay on the default relay")
	}
	for _, body := range [][]byte{
		[]byte(`{"input":"fix the image upload code","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"fix this image upload component","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"修复图标加载逻辑","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"创建图片缓存模块","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"做图片缓存模块","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"design an image processing pipeline","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"create an image metadata schema","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"fix image compression","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"implement a function to generate an image","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"write code to create a logo","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"create a metadata schema for an image","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"create a database record for an image","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"fix the upload pipeline for images","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"create a Docker image","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"fix the container image","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"make an OCI image for this service","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"fix the image in the README","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"修复 README 中的图片","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"edit this image","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"修复这张图片","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"draw a conclusion from these logs","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"draw a random sample from the distribution","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"draw a button component using canvas code","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"实现生成图片接口","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"绘制 React 组件代码","tools":[{"type":"image_generation"}]}`),
	} {
		if decision := decideRelayRoute(body, profile); decision.useImageAPI {
			t.Fatalf("image-related coding request must stay on the default relay: body=%s decision=%#v", body, decision)
		}
	}
	for _, body := range [][]byte{
		[]byte(`{"input":"generate an image of an API gateway","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"生成一张 API 服务架构图","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"generate an API architecture diagram","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"design an API logo","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"draw me a cat","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"use Imagegen to draw a cat","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":"帮我画一只猫","tools":[{"type":"image_generation"}]}`),
	} {
		if decision := decideRelayRoute(body, profile); !decision.useImageAPI {
			t.Fatalf("real image editing request should use the image relay: body=%s decision=%#v", body, decision)
		}
	}
	for _, body := range [][]byte{
		[]byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"},{"type":"input_text","text":"修复这张图片"}]}],"tools":[{"type":"image_generation"}]}`),
		[]byte(`{"input":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.test/source.png"},{"type":"input_text","text":"edit this image"}]}],"tools":[{"type":"image_generation"}]}`),
	} {
		if decision := decideRelayRoute(body, profile); !decision.useImageAPI {
			t.Fatalf("image edit with a current image input should use the image relay: body=%s decision=%#v", body, decision)
		}
	}
	endpoint := decideRelayRouteForPath("/v1/images/generations", []byte(`{"prompt":"draw a lighthouse"}`), profile)
	if !endpoint.useImageAPI || endpoint.reason != "image_endpoint" {
		t.Fatalf("the image generation endpoint should use the image relay: %#v", endpoint)
	}
	responseCall := decideRelayRouteForPath("/v1/responses", []byte(`{"input":[{"type":"image_generation_call","id":"ig_123"}]}`), profile)
	if !responseCall.useImageAPI || responseCall.reason != "image_generation_call" {
		t.Fatalf("a Responses image_generation_call should use the image relay: %#v", responseCall)
	}
	plainResponse := decideRelayRouteForPath("/v1/responses", []byte(`{"input":"implement a parser"}`), profile)
	if plainResponse.useImageAPI {
		t.Fatalf("a normal Responses request must use the default relay: %#v", plainResponse)
	}
	continuedCoding := decideRelayRouteForPath("/v1/responses", []byte(`{"input":[{"type":"image_generation_call","id":"ig_old"},{"role":"user","content":"继续实现解析器，不要生成图片"}],"tools":[{"type":"image_generation"}]}`), profile)
	if continuedCoding.useImageAPI {
		t.Fatalf("an old image call must not route a later coding request to the image relay: %#v", continuedCoding)
	}
}
