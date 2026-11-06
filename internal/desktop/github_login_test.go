package desktop

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

func TestGitHubLoginOpensBrowserAndCopiesCode(t *testing.T) {
	a := fixture(t)
	a.lang = "zh"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/github/device":
			io.WriteString(w, `{"device_code":"fixture-device","user_code":"ABCD-EFGH","verification_uri":"https://github.com/login/device","expires_in":600,"interval":1}`)
		case "/v1/auth/github/token":
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a.apiURL = server.URL
	var opened []string
	a.openURL = func(target string) { opened = append(opened, target) }
	a.beginLogin()
	defer func() {
		a.loginCancel()
		deadline := time.Now().Add(5 * time.Second)
		for a.busy["login"] || a.busy["account-drop-own"] {
			a.drain()
			if time.Now().After(deadline) {
				t.Fatal("login worker did not stop")
			}
			time.Sleep(time.Millisecond)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for a.auth.UserCode == "" {
		a.drain()
		if time.Now().After(deadline) {
			t.Fatal("login did not deliver authorization")
		}
		time.Sleep(time.Millisecond)
	}
	if len(opened) != 1 {
		t.Fatal("login did not automatically open the browser exactly once")
	}
	target, err := url.Parse(opened[0])
	if err != nil || target.Host != "github.com" || target.Path != "/login/device" || target.Query().Get("user_code") != a.auth.UserCode {
		t.Fatal("authorization page did not include the user code")
	}
	a.openSettings("sync")
	var copied string
	var copyErr error
	a.writeClipboard = func(text string) error { copied = text; return copyErr }
	tt := ui.NewTester(func(c *ui.Context) { a.theme(c); a.settingsView(c) }, 800, 780)
	click(t, tt, "复制验证码")
	if copied != a.auth.UserCode || !tt.HasText("验证码已复制") {
		t.Fatal("copy did not write the user code with success feedback")
	}
	copyErr = errors.New("fixture clipboard failure")
	click(t, tt, "复制验证码")
	if !tt.HasText("复制失败") || tt.HasText("验证码已复制") {
		t.Fatal("failed copy reported success")
	}
	click(t, tt, "重新打开 GitHub 授权页")
	if len(opened) != 2 || opened[1] != opened[0] {
		t.Fatal("reopen did not return to the authorization page")
	}
}
