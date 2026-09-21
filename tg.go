package main

// gotd 客户端集成：代理、会话、交互登录、目标解析、--list-chats、发送媒体。
// 对应 tgup.py 的「连接/目标」与 send_one 的发送部分。

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	gotdlog "github.com/gotd/log"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"golang.org/x/net/proxy"
)

// ---- 代理 ---------------------------------------------------------------

// proxyResolver 把 socks5:// / http:// 代理串转成 gotd 的 DC 解析器。
func proxyResolver(proxyURL string) (dcs.Resolver, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return nil, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("代理地址无法解析: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "socks5"
	}
	switch scheme {
	case "socks5":
		var authInfo *proxy.Auth
		if u.User != nil {
			pwd, _ := u.User.Password()
			authInfo = &proxy.Auth{User: u.User.Username(), Password: pwd}
		}
		addr := u.Host
		if !strings.Contains(addr, ":") {
			addr += ":1080"
		}
		d, err := proxy.SOCKS5("tcp", addr, authInfo, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("SOCKS5 拨号器创建失败: %w", err)
		}
		cd, ok := d.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("SOCKS5 拨号器不支持 ContextDialer")
		}
		return dcs.Plain(dcs.PlainOptions{Dial: cd.DialContext}), nil
	case "http":
		return dcs.Plain(dcs.PlainOptions{Dial: httpConnectDialer(u)}), nil
	default:
		// Python 版还支持 socks4；x/net 无对应实现，明确报错而不是静默直连
		return nil, fmt.Errorf("不支持的代理协议: %s（仅支持 socks5 / http）", scheme)
	}
}

// httpConnectDialer 实现 HTTP CONNECT 隧道拨号。
func httpConnectDialer(u *url.URL) dcs.DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host := u.Host
		if !strings.Contains(host, ":") {
			host += ":8080"
		}
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", host)
		if err != nil {
			return nil, err
		}
		req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
		if u.User != nil {
			pwd, _ := u.User.Password()
			req += "Proxy-Authorization: Basic " +
				base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pwd)) + "\r\n"
		}
		req += "\r\n"
		if _, err := conn.Write([]byte(req)); err != nil {
			conn.Close()
			return nil, err
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
		if err != nil {
			conn.Close()
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("HTTP 代理返回 %s", resp.Status)
		}
		// 代理应答之后可能已有字节缓冲，包一层避免丢数据
		return &bufferedConn{Conn: conn, r: br}, nil
	}
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// ---- 会话 ----------------------------------------------------------------

// importTelethonSession 把 Telethon StringSession 转存成 gotd 会话文件。
func importTelethonSession(ctx context.Context, sessionPath, s string) error {
	data, err := session.TelethonSession(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("Telethon StringSession 解析失败: %w", err)
	}
	ld := session.Loader{Storage: &session.FileStorage{Path: sessionPath}}
	if err := ld.Save(ctx, data); err != nil {
		return err
	}
	logf("已从 Telethon StringSession 导入会话 → %s", sessionPath)
	return nil
}

// ---- 交互登录 -------------------------------------------------------------

type termAuth struct{ phone string }

func promptLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return "", errors.New("未读取到输入")
	}
	return strings.TrimSpace(sc.Text()), nil
}

func (t termAuth) Phone(ctx context.Context) (string, error) {
	if t.phone != "" {
		return t.phone, nil
	}
	return promptLine("请输入手机号（含国际区号，如 +8613xxxxxxxxx）: ")
}

func (t termAuth) Password(ctx context.Context) (string, error) {
	return promptLine("请输入两步验证密码: ")
}

func (t termAuth) Code(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
	return promptLine("请输入收到的验证码: ")
}

func (t termAuth) AcceptTermsOfService(ctx context.Context, tos tg.HelpTermsOfService) error {
	return nil // 不自动同意任何条款；新注册账号请先在官方客户端完成注册
}

func (t termAuth) SignUp(ctx context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("该手机号尚未注册 Telegram，请先在官方客户端注册")
}

// ---- 目标解析 --------------------------------------------------------------

// resolveTarget 把 me / @username / 数字 id 解析成 InputPeer。
// gotd 不像 Telethon 那样在会话里缓存实体，数字 id 需要扫一遍会话列表拿 access_hash。
func resolveTarget(ctx context.Context, api *tg.Client, target string) (tg.InputPeerClass, error) {
	t := strings.TrimSpace(target)
	switch strings.ToLower(t) {
	case "me", "self", "saved":
		return &tg.InputPeerSelf{}, nil
	}
	if n, err := strconv.ParseInt(t, 10, 64); err == nil {
		peer, perr := findByDialogScan(ctx, api, n)
		if perr != nil {
			return nil, fmt.Errorf("无法解析目标 %q: %w\n"+
				"提示：先跑 --list-chats 查看可用目标，或改用 @username。", target, perr)
		}
		return peer, nil
	}
	username := strings.TrimPrefix(t, "@")
	res, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: username,
	})
	if err != nil {
		return nil, fmt.Errorf("无法解析目标 %q: %w\n"+
			"提示：先跑 --list-chats 让会话缓存该实体，或改用 @username。", target, err)
	}
	peer, ok := peerFromResolved(res)
	if !ok {
		return nil, fmt.Errorf("无法解析目标 %q: 服务器返回的结果里找不到对应实体", target)
	}
	return peer, nil
}

func peerFromResolved(res *tg.ContactsResolvedPeer) (tg.InputPeerClass, bool) {
	switch p := res.Peer.(type) {
	case *tg.PeerUser:
		for _, uc := range res.Users {
			if u, ok := uc.(*tg.User); ok && u.ID == p.UserID {
				return &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash}, true
			}
		}
		return &tg.InputPeerUser{UserID: p.UserID}, true
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: p.ChatID}, true
	case *tg.PeerChannel:
		for _, cc := range res.Chats {
			if c, ok := cc.(*tg.Channel); ok && c.ID == p.ChannelID {
				return &tg.InputPeerChannel{ChannelID: c.ID, AccessHash: c.AccessHash}, true
			}
		}
		return &tg.InputPeerChannel{ChannelID: p.ChannelID}, true
	}
	return nil, false
}

// peerForID 在已收集的 users/chats 里找 want（支持 Telethon 风格标记 id）。
func peerForID(want int64, users map[int64]*tg.User, chats map[int64]tg.ChatClass) (tg.InputPeerClass, bool) {
	if want > 0 {
		if u, ok := users[want]; ok {
			return &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash}, true
		}
		if c, ok := chats[want]; ok {
			if ch, isCh := c.(*tg.Channel); isCh {
				return &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}, true
			}
		}
		return nil, false
	}
	id := -want
	if id >= 1000000000000 { // -100 前缀的频道标记 id
		id -= 1000000000000
	}
	if c, ok := chats[id]; ok {
		if ch, isCh := c.(*tg.Channel); isCh {
			return &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}, true
		}
		return &tg.InputPeerChat{ChatID: id}, true
	}
	return nil, false
}

// findByDialogScan 翻会话列表找数字 id 对应的实体（含 Telethon 风格的 -100 前缀）。
func findByDialogScan(ctx context.Context, api *tg.Client, want int64) (tg.InputPeerClass, error) {
	users := map[int64]*tg.User{}
	chats := map[int64]tg.ChatClass{}
	offsetDate, offsetID := 0, 0
	var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
	const pages = 10 // 最多翻 10×100 个会话
	for page := 0; page < pages; page++ {
		res, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate, OffsetID: offsetID, OffsetPeer: offsetPeer,
			Limit: 100,
		})
		if err != nil {
			return nil, err
		}
		dlg := dialogsFromResult(res)
		if dlg == nil || len(dlg.Dialogs) == 0 {
			break
		}
		for _, uc := range dlg.Users {
			if u, ok := uc.(*tg.User); ok {
				users[u.ID] = u
			}
		}
		for _, cc := range dlg.Chats {
			switch c := cc.(type) {
			case *tg.Chat:
				chats[c.ID] = c
			case *tg.ChatForbidden:
				chats[c.ID] = c
			case *tg.Channel:
				chats[c.ID] = c
			}
		}
		if peer, ok := peerForID(want, users, chats); ok {
			return peer, nil
		}
		// 推进分页
		last, ok := dlg.Dialogs[len(dlg.Dialogs)-1].(*tg.Dialog)
		if !ok {
			break
		}
		offsetPeer = inputPeerFromPeer(last.Peer)
		for _, mc := range dlg.Messages {
			if m, ok := mc.(*tg.Message); ok {
				offsetDate, offsetID = m.Date, m.ID
				break
			}
		}
		if len(dlg.Dialogs) < 100 {
			break
		}
	}
	return nil, fmt.Errorf("未在会话列表中找到 id=%d", want)
}

func dialogsFromResult(res tg.MessagesDialogsClass) *tg.MessagesDialogs {
	switch r := res.(type) {
	case *tg.MessagesDialogs:
		return r
	case *tg.MessagesDialogsSlice:
		return &tg.MessagesDialogs{Dialogs: r.Dialogs, Messages: r.Messages,
			Users: r.Users, Chats: r.Chats}
	}
	return nil
}

func inputPeerFromPeer(p tg.PeerClass) tg.InputPeerClass {
	switch v := p.(type) {
	case *tg.PeerUser:
		return &tg.InputPeerUser{UserID: v.UserID}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: v.ChatID}
	case *tg.PeerChannel:
		return &tg.InputPeerChannel{ChannelID: v.ChannelID}
	}
	return &tg.InputPeerEmpty{}
}

// ---- 会话列表 --------------------------------------------------------------

// listChats 打印会话列表到 stdout，格式与 Python 版对齐（频道用 -100 前缀标记 id）。
func listChats(ctx context.Context, api *tg.Client, limit int) error {
	fmt.Printf("%16s  %-6s %s\n", "ID", "类型", "名称")
	fmt.Println(strings.Repeat("-", 70))
	offsetDate, offsetID := 0, 0
	var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
	shown := 0
	for shown < limit {
		res, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate, OffsetID: offsetID, OffsetPeer: offsetPeer,
			Limit: min(100, limit-shown),
		})
		if err != nil {
			return err
		}
		dlg := dialogsFromResult(res)
		if dlg == nil || len(dlg.Dialogs) == 0 {
			break
		}
		userByID := map[int64]*tg.User{}
		for _, uc := range dlg.Users {
			if u, ok := uc.(*tg.User); ok {
				userByID[u.ID] = u
			}
		}
		chatByID := map[int64]tg.ChatClass{}
		for _, cc := range dlg.Chats {
			switch c := cc.(type) {
			case *tg.Chat:
				chatByID[c.ID] = c
			case *tg.Channel:
				chatByID[c.ID] = c
			}
		}
		for _, dc := range dlg.Dialogs {
			d, ok := dc.(*tg.Dialog)
			if !ok {
				continue
			}
			var id int64
			var kind, name string
			switch p := d.Peer.(type) {
			case *tg.PeerUser:
				id = p.UserID
				kind = "私聊"
				if u := userByID[p.UserID]; u != nil {
					name = strings.TrimSpace(u.FirstName + " " + u.LastName)
				}
			case *tg.PeerChat:
				id = -p.ChatID
				kind = "群组"
				if c := chatByID[p.ChatID]; c != nil {
					if ch, ok := c.(*tg.Chat); ok {
						name = ch.Title
					}
				}
			case *tg.PeerChannel:
				id = -(1000000000000 + p.ChannelID)
				kind = "频道"
				if c := chatByID[p.ChannelID]; c != nil {
					if ch, ok := c.(*tg.Channel); ok {
						name = ch.Title
					}
				}
			default:
				continue
			}
			fmt.Printf("%16d  %-6s %s\n", id, kind, name)
			shown++
			if shown >= limit {
				break
			}
		}
		if len(dlg.Dialogs) < 100 {
			break
		}
		if last, ok := dlg.Dialogs[len(dlg.Dialogs)-1].(*tg.Dialog); ok {
			offsetPeer = inputPeerFromPeer(last.Peer)
		}
		for _, mc := range dlg.Messages {
			if m, ok := mc.(*tg.Message); ok {
				offsetDate, offsetID = m.Date, m.ID
				break
			}
		}
	}
	return nil
}

// ---- 发送 ------------------------------------------------------------------

// extractMessageID 从 sendMedia 的返回里挖出 message_id。
func extractMessageID(updates tg.UpdatesClass) (int64, bool) {
	var list []tg.UpdateClass
	switch u := updates.(type) {
	case *tg.Updates:
		list = u.Updates
	case *tg.UpdatesCombined:
		list = u.Updates
	case *tg.UpdateShortSentMessage:
		return int64(u.ID), true
	default:
		return 0, false
	}
	for _, x := range list {
		if id, ok := x.(*tg.UpdateMessageID); ok {
			return int64(id.ID), true
		}
	}
	for _, x := range list {
		if nm, ok := x.(*tg.UpdateNewMessage); ok {
			if m, ok := nm.Message.(*tg.Message); ok {
				return int64(m.ID), true
			}
		}
	}
	return 0, false
}

// isFloodWait 抽出 FloodWait 的秒数。
func isFloodWait(err error) (time.Duration, bool) {
	return tgerr.AsFloodWait(err)
}

// isFilePartMissing 判断 FILE_PART_MISSING 类错误。
func isFilePartMissing(err error) bool {
	return tgerr.Is(err, "FILE_PART_MISSING")
}

func randInt64() int64 { return rand.Int64() }

// ---- verbose 日志 -----------------------------------------------------------
//
// gotd/log 只定义接口，这里给 --verbose 提供一个最简 stderr 实现。

type stderrLogger struct{}

func (stderrLogger) Enabled(_ context.Context, _ gotdlog.Level) bool { return true }

func (stderrLogger) Log(_ context.Context, level gotdlog.Level, msg string, attrs ...gotdlog.Attr) {
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Fprintf(os.Stderr, "  [%s] %s", level, msg)
	for _, a := range attrs {
		fmt.Fprintf(os.Stderr, " %s=%v", a.Key, a.Value)
	}
	fmt.Fprintln(os.Stderr)
}

func verboseLogger() gotdlog.Logger { return stderrLogger{} }
