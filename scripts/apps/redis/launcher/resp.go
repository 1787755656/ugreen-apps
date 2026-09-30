package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 一个刚好够用的 RESP（Redis 序列化协议）客户端。
//
// 为什么自己写而不是打包 redis-cli：
//
//   - 管理页每 5 秒查一次状态。走 redis-cli 就是【每次 fork 一个进程】，
//     还得去解析它给人看的文本输出 —— 那个输出的格式是会随版本变的，
//     而且出错时它只把话写到 stderr，很难分清"连不上"和"命令报错"。
//   - 密码。redis-cli 的 -a 会把密码放进命令行（进程的 cmdline 谁都读得到），
//     绕开要靠 REDISCLI_AUTH 环境变量，又多一处得记住的坑。
//   - 少打包一个 1.2MB 的二进制，而且 redis-server 就成了整个应用【唯一】
//     被 exec 的东西。
//
// RESP2 足够了，我们不发 HELLO 3。协议本身很小：
//
//	+OK\r\n              简单字符串
//	-ERR something\r\n   错误
//	:123\r\n             整数
//	$5\r\nhello\r\n      批量字符串（$-1 表示 nil）
//	*2\r\n...            数组（*-1 表示 nil）

// Value 是一条 RESP 回复。
type Value struct {
	Type byte // '+' '-' ':' '$' '*'
	Str  string
	Int  int64
	Arr  []Value
	Nil  bool
}

// String 把回复压成一行人能看的文本，用于管理页上的「运行命令」。
func (v Value) String() string {
	switch v.Type {
	case '+', '-', '$':
		if v.Nil {
			return "(nil)"
		}
		return v.Str
	case ':':
		return strconv.FormatInt(v.Int, 10)
	case '*':
		if v.Nil {
			return "(nil)"
		}
		if len(v.Arr) == 0 {
			return "(empty array)"
		}
		var sb strings.Builder
		for i, e := range v.Arr {
			if i > 0 {
				sb.WriteByte('\n')
			}
			// 数组元素带序号，和 redis-cli 的习惯一致，多行内容也看得清
			fmt.Fprintf(&sb, "%d) %s", i+1, strings.ReplaceAll(e.String(), "\n", "\n   "))
		}
		return sb.String()
	}
	return ""
}

// RedisError 是服务端返回的 -ERR 之类。单独一个类型，好让调用方区分
// "命令被拒绝"和"连不上" —— 这两种在管理页上要给完全不同的提示。
type RedisError struct{ Msg string }

func (e *RedisError) Error() string { return e.Msg }

// Client 是一条带锁的长连接。
//
// 只用一条连接：管理壳对 Redis 的访问全是低频的小命令，连接池只会带来
// "哪条连接认证过了"这类没必要的复杂度。锁保证命令不会交叉。
type Client struct {
	mu       sync.Mutex
	conn     net.Conn
	rd       *bufio.Reader
	network  string
	addr     string
	password string
	timeout  time.Duration
}

func NewClient(network, addr, password string) *Client {
	return &Client{network: network, addr: addr, password: password, timeout: 5 * time.Second}
}

// SetAuth 改密码后要跟着换，否则下次重连会认证失败。
func (c *Client) SetAuth(password string) {
	c.mu.Lock()
	c.password = password
	// 当前这条连接是用旧密码认证的，它还能用；但既然密码变了，
	// 干脆断掉重来，免得"这条连接还活着"和"密码已经换了"两种事实并存。
	c.closeLocked()
	c.mu.Unlock()
}

func (c *Client) SetTarget(network, addr string) {
	c.mu.Lock()
	c.network, c.addr = network, addr
	c.closeLocked()
	c.mu.Unlock()
}

func (c *Client) Close() {
	c.mu.Lock()
	c.closeLocked()
	c.mu.Unlock()
}

func (c *Client) closeLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.rd = nil
	}
}

// Do 发一条命令并读回复。
//
// 连接坏了会【重连一次再试】：Redis 重启、超过 timeout 被服务端断开都很常见，
// 让调用方为此写重试是把偶然的网络事实泄露到业务代码里。
// 但只重试一次，而且只在"写/读出错"时重试 —— 服务端明确回了错误的不重试，
// 那种情况重发一遍毫无意义（还可能把有副作用的命令执行两次）。
func (c *Client) Do(ctx context.Context, args ...string) (Value, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(args) == 0 {
		return Value{}, errors.New("空命令")
	}
	v, err := c.doLocked(ctx, args)
	if err != nil && !isRedisError(err) {
		c.closeLocked()
		v, err = c.doLocked(ctx, args)
	}
	return v, err
}

func isRedisError(err error) bool {
	var re *RedisError
	return errors.As(err, &re)
}

func (c *Client) doLocked(ctx context.Context, args []string) (Value, error) {
	if err := c.connectLocked(ctx); err != nil {
		return Value{}, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(dl)
	} else {
		_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	}
	if err := writeCommand(c.conn, args); err != nil {
		return Value{}, err
	}
	return readValue(c.rd)
}

func (c *Client) connectLocked(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	d := net.Dialer{Timeout: c.timeout}
	conn, err := d.DialContext(ctx, c.network, c.addr)
	if err != nil {
		return err
	}
	c.conn = conn
	c.rd = bufio.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(c.timeout))

	if c.password != "" {
		if err := writeCommand(conn, []string{"AUTH", c.password}); err != nil {
			c.closeLocked()
			return err
		}
		v, err := readValue(c.rd)
		if err != nil {
			// 服务端【没有】设密码时 AUTH 会被拒。这不该让整条路断掉 ——
			// 用户可能刚把密码删掉，或者配置和实际状态一时不同步。
			if isRedisError(err) && strings.Contains(strings.ToLower(err.Error()),
				"without any password") {
				return nil
			}
			c.closeLocked()
			return err
		}
		_ = v
	}
	return nil
}

func writeCommand(w io.Writer, args []string) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&sb, "$%d\r\n%s\r\n", len(a), a)
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

// maxBulkLen 限制单条批量回复的大小。
//
// 【必须有】：管理页上的「运行命令」能发出任意命令，一条 `LRANGE biglist 0 -1`
// 或者 `DEBUG JMAP` 就能让服务端回几个 G。没有上限的话管理壳会跟着把内存吃光，
// 而它一死，用户连"停止数据库"的按钮都点不到了。
const maxBulkLen = 8 << 20

// maxArrayLen 同理，挡住元素数量极多的数组回复。
const maxArrayLen = 100000

func readValue(rd *bufio.Reader) (Value, error) {
	line, err := readLine(rd)
	if err != nil {
		return Value{}, err
	}
	if len(line) == 0 {
		return Value{}, errors.New("收到空的协议行")
	}
	typ, body := line[0], line[1:]
	switch typ {
	case '+':
		return Value{Type: '+', Str: body}, nil
	case '-':
		return Value{}, &RedisError{Msg: body}
	case ':':
		n, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return Value{}, fmt.Errorf("整数回复解析失败 %q: %w", body, err)
		}
		return Value{Type: ':', Int: n}, nil
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil {
			return Value{}, fmt.Errorf("批量长度解析失败 %q: %w", body, err)
		}
		if n < 0 {
			return Value{Type: '$', Nil: true}, nil
		}
		if n > maxBulkLen {
			return Value{}, fmt.Errorf("回复太大（%d 字节，上限 %d）——"+
				"这条命令的结果不适合在管理页上看", n, maxBulkLen)
		}
		buf := make([]byte, n+2) // 连同结尾的 \r\n 一起读掉
		if _, err := io.ReadFull(rd, buf); err != nil {
			return Value{}, err
		}
		return Value{Type: '$', Str: string(buf[:n])}, nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil {
			return Value{}, fmt.Errorf("数组长度解析失败 %q: %w", body, err)
		}
		if n < 0 {
			return Value{Type: '*', Nil: true}, nil
		}
		if n > maxArrayLen {
			return Value{}, fmt.Errorf("回复的元素太多（%d 个，上限 %d）", n, maxArrayLen)
		}
		arr := make([]Value, 0, n)
		for i := 0; i < n; i++ {
			e, err := readValue(rd)
			if err != nil {
				// 数组元素里的 -ERR 是合法内容（比如 EXEC 的结果），
				// 不能让它把整个数组的读取中断掉，否则连接里会残留没读完的字节。
				var re *RedisError
				if errors.As(err, &re) {
					arr = append(arr, Value{Type: '-', Str: re.Msg})
					continue
				}
				return Value{}, err
			}
			arr = append(arr, e)
		}
		return Value{Type: '*', Arr: arr}, nil
	}
	return Value{}, fmt.Errorf("不认识的 RESP 类型 %q", string(typ))
}

// readLine 读一行并去掉结尾的 \r\n。
func readLine(rd *bufio.Reader) (string, error) {
	line, err := rd.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ---- 命令行拆分 ---------------------------------------------------------

// SplitArgs 把用户在管理页上敲的一行命令拆成参数，规则和 redis-cli 一致：
// 空白分隔，支持单引号和双引号，双引号里支持 \xNN 和常见转义。
//
// 【不能简单地按空格 split】：`SET greeting "hello world"` 会被拆成四段，
// 存进去的值是 "hello 而不是 hello world —— 而且这种错不会报任何错，
// 用户是过一阵子发现数据不对才回来找的。
func SplitArgs(line string) ([]string, error) {
	var args []string
	i := 0
	for {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t' ||
			line[i] == '\n' || line[i] == '\r') {
			i++
		}
		if i >= len(line) {
			return args, nil
		}
		var cur strings.Builder
		inQ, inSQ, done := false, false, false
		for i < len(line) && !done {
			ch := line[i]
			switch {
			case inQ:
				if ch == '\\' && i+3 < len(line) && line[i+1] == 'x' &&
					isHex(line[i+2]) && isHex(line[i+3]) {
					cur.WriteByte(hexVal(line[i+2])<<4 | hexVal(line[i+3]))
					i += 4
					continue
				}
				if ch == '\\' && i+1 < len(line) {
					i++
					switch line[i] {
					case 'n':
						cur.WriteByte('\n')
					case 'r':
						cur.WriteByte('\r')
					case 't':
						cur.WriteByte('\t')
					case 'b':
						cur.WriteByte('\b')
					case 'a':
						cur.WriteByte('\a')
					default:
						cur.WriteByte(line[i])
					}
					i++
					continue
				}
				if ch == '"' {
					// 闭引号后面必须是空白或行尾，和 redis-cli 一样严格 ——
					// 宽松的话 "a"b 会被静默接受成 ab，用户看不出哪里错了
					if i+1 < len(line) && !isSpace(line[i+1]) {
						return nil, fmt.Errorf("引号闭合后必须跟空格（第 %d 个字符处）", i+1)
					}
					inQ = false
					done = true
					i++
					continue
				}
				cur.WriteByte(ch)
				i++
			case inSQ:
				if ch == '\\' && i+1 < len(line) && line[i+1] == '\'' {
					cur.WriteByte('\'')
					i += 2
					continue
				}
				if ch == '\'' {
					if i+1 < len(line) && !isSpace(line[i+1]) {
						return nil, fmt.Errorf("引号闭合后必须跟空格（第 %d 个字符处）", i+1)
					}
					inSQ = false
					done = true
					i++
					continue
				}
				cur.WriteByte(ch)
				i++
			default:
				switch ch {
				case ' ', '\t', '\n', '\r':
					done = true
					i++
				case '"':
					inQ = true
					i++
				case '\'':
					inSQ = true
					i++
				default:
					cur.WriteByte(ch)
					i++
				}
			}
		}
		if inQ || inSQ {
			return nil, errors.New("引号没有闭合")
		}
		args = append(args, cur.String())
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// ---- INFO 解析 ----------------------------------------------------------

// ParseInfo 把 INFO 的输出解析成 map。
// 格式是 "# Section" 分段 + "key:value" 行，注释和空行忽略。
func ParseInfo(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// KeyspaceEntry 是 INFO keyspace 里的一行：db0:keys=12,expires=3,avg_ttl=0
type KeyspaceEntry struct {
	DB      string `json:"db"`
	Keys    int64  `json:"keys"`
	Expires int64  `json:"expires"`
}

func ParseKeyspace(info map[string]string) []KeyspaceEntry {
	var out []KeyspaceEntry
	for k, v := range info {
		if !strings.HasPrefix(k, "db") {
			continue
		}
		e := KeyspaceEntry{DB: k}
		for _, part := range strings.Split(v, ",") {
			name, val, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			n, _ := strconv.ParseInt(val, 10, 64)
			switch name {
			case "keys":
				e.Keys = n
			case "expires":
				e.Expires = n
			}
		}
		out = append(out, e)
	}
	// 按 db 号排序，不能按字符串 —— 否则 db10 会排在 db2 前面
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && dbNum(out[j].DB) < dbNum(out[j-1].DB); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func dbNum(s string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(s, "db"))
	if err != nil {
		return 1 << 30
	}
	return n
}
