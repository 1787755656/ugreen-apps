package main

// cloud.go talks to the SakuraFrp public API (https://api.natfrp.com/v4),
// authenticated with the user's access key ("token") which we already keep in
// natfrp's config.json. The key stays server-side here and is never sent to
// the browser. This is what powers the in-app tunnel panel / account view, so
// common management doesn't need natfrp's own Web UI.

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const natfrpAPIBase = "https://api.natfrp.com/v4"

var apiClient = &http.Client{
	Timeout: 15 * time.Second,
	// The API is public CA-signed; default transport is fine. (natfrp's LOCAL
	// WebUI is the self-signed one — we don't talk to that here.)
	Transport: &http.Transport{TLSClientConfig: &tls.Config{}},
}

func accessToken() string {
	tok, _ := readConfig()["token"].(string)
	return tok
}

// apiGet / apiPostForm call the SakuraFrp API with the access key as a bearer
// token. Returns the decoded JSON (into out), or an error string suitable for
// showing the user.
func apiGet(path string, out any) (int, error) {
	req, _ := http.NewRequest(http.MethodGet, natfrpAPIBase+path, nil)
	return apiDo(req, out)
}

func apiPostForm(path string, form url.Values, out any) (int, error) {
	req, _ := http.NewRequest(http.MethodPost, natfrpAPIBase+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return apiDo(req, out)
}

func apiDo(req *http.Request, out any) (int, error) {
	req.Header.Set("Authorization", "Bearer "+accessToken())
	resp, err := apiClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if out != nil && len(body) > 0 {
		_ = json.Unmarshal(body, out)
	}
	return resp.StatusCode, nil
}

// --- handlers ---

func handleAccount(w http.ResponseWriter, _ *http.Request) {
	if accessToken() == "" {
		writeErr(w, http.StatusBadRequest, "尚未设置访问密钥")
		return
	}
	var info map[string]any
	code, err := apiGet("/user/info", &info)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "连接樱花 API 失败："+err.Error())
		return
	}
	if code != 200 {
		writeErr(w, http.StatusBadGateway, "樱花 API 返回错误（访问密钥是否正确？）")
		return
	}
	writeJSON(w, info)
}

type tunnelView struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Node       int    `json:"node"`
	NodeName   string `json:"node_name"`
	NodeOnline bool   `json:"node_online"`
	NodeLoad   int    `json:"node_load"` // node load metric from /node/stats
	Online     bool   `json:"online"`
	LocalIP    string `json:"local_ip"`
	LocalPort  int    `json:"local_port"`
	Remote     string `json:"remote"`
	Access     string `json:"access"`     // public connection address, e.g. host:port or http(s)://domain
	AutoStart  bool   `json:"auto_start"` // whether this tunnel auto-runs on THIS machine
}

// tunnelAccess builds the public connection address natfrp reports after a
// tunnel connects: node host + remote port for tcp/udp, or the bound domain
// (with scheme) for http/https. Other types (wol/etcp/eudp) have no simple
// address.
func tunnelAccess(typ, nodeHost, remote string) string {
	switch typ {
	case "http":
		return "http://" + remote
	case "https":
		return "https://" + remote
	case "tcp", "udp":
		if nodeHost != "" && remote != "" {
			return nodeHost + ":" + remote
		}
	}
	return ""
}

func handleTunnels(w http.ResponseWriter, _ *http.Request) {
	if accessToken() == "" {
		writeErr(w, http.StatusBadRequest, "尚未设置访问密钥")
		return
	}

	var raw []struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		Type      string `json:"type"`
		Node      int    `json:"node"`
		Online    bool   `json:"online"`
		LocalIP   string `json:"local_ip"`
		LocalPort int    `json:"local_port"`
		Remote    string `json:"remote"`
	}
	if code, err := apiGet("/tunnels", &raw); err != nil || code != 200 {
		writeErr(w, http.StatusBadGateway, "获取隧道列表失败（访问密钥是否正确？）")
		return
	}

	// node id -> name/host (best effort; ignore failures)
	nodeNames := map[int]string{}
	nodeHosts := map[int]string{}
	var nodes map[string]struct {
		Name string `json:"name"`
		Host string `json:"host"`
	}
	if _, err := apiGet("/nodes", &nodes); err == nil {
		for idStr, n := range nodes {
			id := atoiSafe(idStr)
			nodeNames[id] = n.Name
			nodeHosts[id] = n.Host
		}
	}

	// node id -> live status/load (best effort)
	type nstat struct {
		online int
		load   int
	}
	nodeStats := map[int]nstat{}
	var ns struct {
		Nodes []struct {
			ID     int `json:"id"`
			Online int `json:"online"`
			Load   int `json:"load"`
		} `json:"nodes"`
	}
	if _, err := apiGet("/node/stats", &ns); err == nil {
		for _, n := range ns.Nodes {
			nodeStats[n.ID] = nstat{n.Online, n.Load}
		}
	}

	auto := autoStartSet(readConfig())
	out := make([]tunnelView, 0, len(raw))
	for _, t := range raw {
		st := nodeStats[t.Node]
		out = append(out, tunnelView{
			ID: t.ID, Name: t.Name, Type: t.Type, Node: t.Node, NodeName: nodeNames[t.Node],
			NodeOnline: st.online >= 0, NodeLoad: st.load,
			Online: t.Online, LocalIP: t.LocalIP, LocalPort: t.LocalPort, Remote: t.Remote,
			Access:    tunnelAccess(t.Type, nodeHosts[t.Node], t.Remote),
			AutoStart: auto[t.ID],
		})
	}
	writeJSON(w, out)
}

// handleDataPlans proxies the user's traffic packages (name / total /
// remaining / expiry) — read-only account info.
func handleDataPlans(w http.ResponseWriter, _ *http.Request) {
	if accessToken() == "" {
		writeErr(w, http.StatusBadRequest, "尚未设置访问密钥")
		return
	}
	var plans []map[string]any
	if code, err := apiGet("/user/data_plans?status=valid", &plans); err != nil || code != 200 {
		writeErr(w, http.StatusBadGateway, "获取流量包失败")
		return
	}
	if plans == nil {
		plans = []map[string]any{}
	}
	writeJSON(w, plans)
}

type autostartRequest struct {
	ID      int  `json:"id"`
	Enabled bool `json:"enabled"`
}

// handleTunnelAutostart adds/removes a tunnel ID in config's
// auto_start_tunnels (the tunnels natfrp connects on startup on this machine)
// and restarts natfrp so it takes effect. This is our "run this tunnel on
// this NAS" control — the SakuraFrp cloud API has no live start/stop.
func handleTunnelAutostart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req autostartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
		writeErr(w, http.StatusBadRequest, "请求格式不正确")
		return
	}

	cfg := readConfig()
	set := autoStartSet(cfg)
	if req.Enabled {
		set[req.ID] = true
	} else {
		delete(set, req.ID)
	}
	ids := make([]int, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	cfg["auto_start_tunnels"] = ids
	if err := writeConfig(cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	restartNatfrp()
	writeJSON(w, map[string]any{"ok": true})
}

type remoteRequest struct {
	Enabled     *bool `json:"enabled"`
	AllowConfig *bool `json:"allow_config"`
	AllowUpdate *bool `json:"allow_update"`
}

// handleRemote enables/disables remote management (natfrp.com/remote/v2) and
// toggles the allow-config / allow-update flags. The end-to-end PASSWORD is
// NOT set here: natfrp stores a key derived from it (SHA256×10000 with an
// undisclosed construction, done server-side in its packed binary when set
// via its own RPC), and writing plaintext to config.json does NOT work — so
// the password must be set once in natfrp's own Web UI. Enabling requires
// that key to already exist.
func handleRemote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req remoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不正确")
		return
	}

	cfg := readConfig()
	if req.Enabled != nil {
		key, _ := cfg["remote_management_key"].(string)
		if *req.Enabled && key == "" {
			writeErr(w, http.StatusBadRequest, "请先在樱花 Web UI 里设置远程管理密码，再启用")
			return
		}
		cfg["remote_management"] = *req.Enabled
	}
	if req.AllowConfig != nil {
		cfg["remote_management_allow_config"] = *req.AllowConfig
	}
	if req.AllowUpdate != nil {
		cfg["remote_management_allow_update"] = *req.AllowUpdate
	}
	if err := writeConfig(cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	restartNatfrp()
	writeJSON(w, map[string]any{"ok": true})
}

// --- helpers ---

// autoStartSet reads config's auto_start_tunnels (a JSON array of tunnel IDs,
// possibly null) into a set of ints.
func autoStartSet(cfg map[string]any) map[int]bool {
	set := map[int]bool{}
	if arr, ok := cfg["auto_start_tunnels"].([]any); ok {
		for _, v := range arr {
			if f, ok := v.(float64); ok {
				set[int(f)] = true
			}
		}
	}
	return set
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
