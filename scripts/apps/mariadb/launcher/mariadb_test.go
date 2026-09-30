package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 密码是用户输入、又要拼进 SQL 语句里的，转义错了就不只是"功能不对"，
// 而是可以从管理页面注入任意 SQL（虽然那本来就是个满权限的通道，
// 但语句被截断会让用户在毫不知情的情况下留下一个密码不对的 root 账号）。
func TestRootPasswordSQLEscaping(t *testing.T) {
	cases := []struct {
		name string
		pw   string
	}{
		{"普通", "correct horse battery"},
		{"单引号", "abc'def'ghi"},
		{"反斜杠", `abc\def\`},
		{"反斜杠加引号", `a\'; DROP TABLE mysql.global_priv; -- `},
		{"中文", "我的数据库密码很安全"},
		{"注释符", "pass--word/*x*/#y"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, err := rootPasswordSQL(c.pw)
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			for i, line := range strings.Split(strings.TrimRight(sql, "\n"), "\n") {
				// 每一行都必须是完整的一条语句（以分号结尾）：
				// 密码里的引号没转义的话，会把某一行提前截断。
				if !strings.HasSuffix(line, ";") {
					t.Errorf("第 %d 行没有以分号结尾，密码可能截断了语句:\n%s", i+1, line)
					continue
				}
				// 真正要验的性质：按 MySQL 的规则把字面量解回来，必须等于原密码。
				//
				// 别用"单引号数量必须是偶数"这种启发式 —— 它是错的：
				// 转义后的 \' 本来就会让计数变成奇数（`a\'` → `'a\\\'`），
				// 而那恰恰是【正确】的转义。我第一版就是这么写的，
				// 结果测试报了一堆假警报。
				idx := strings.Index(line, "IDENTIFIED BY ")
				if idx < 0 {
					continue // GRANT / DELETE / FLUSH 那几行没有密码
				}
				lit := strings.TrimSuffix(line[idx+len("IDENTIFIED BY "):], ";")
				got, rest, err := unquoteMySQL(lit)
				if err != nil {
					t.Errorf("第 %d 行的字面量解析失败（%v）:\n%s", i+1, err, line)
					continue
				}
				if rest != "" {
					t.Errorf("第 %d 行字面量之后还有残留 %q —— 语句被密码截断了:\n%s",
						i+1, rest, line)
				}
				if got != c.pw {
					t.Errorf("第 %d 行解回来的密码是 %q，期望 %q", i+1, got, c.pw)
				}
			}
			// 开头 FLUSH + 四个 host 各三条语句 + 删匿名账号 + 收尾 FLUSH
			if got, want := strings.Count(sql, ";\n"), 1+4*3+2; got != want {
				t.Errorf("语句条数 = %d，期望 %d", got, want)
			}
			if !strings.Contains(sql, "'root'@'%'") {
				t.Error("没有为远程访问建 root@'%%'")
			}
		})
	}
}

// unquoteMySQL 把一个 MySQL 单引号字面量解回原字符串，
// 返回 (内容, 字面量结束之后剩下的部分, 错误)。
// 这是测试用的"另一半实现"：拿它和 quoteSQLString 对着跑，
// 才能证明转义是【可逆】的，而不是只看着像对。
func unquoteMySQL(s string) (string, string, error) {
	if len(s) == 0 || s[0] != '\'' {
		return "", "", errNotQuoted
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\'':
			return b.String(), s[i+1:], nil
		case '\\':
			i++
			if i >= len(s) {
				return "", "", errUnterminated
			}
			switch s[i] {
			case '0':
				b.WriteByte(0)
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			default:
				// MySQL 对未知转义序列的规则是"去掉反斜杠、保留字符本身"
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(s[i])
		}
	}
	return "", "", errUnterminated
}

type unquoteErr string

func (e unquoteErr) Error() string { return string(e) }

const (
	errNotQuoted    = unquoteErr("不是以单引号开头的字面量")
	errUnterminated = unquoteErr("字面量没有闭合")
)

func TestQuoteSQLString(t *testing.T) {
	cases := map[string]string{
		"abc":    `'abc'`,
		"a'b":    `'a\'b'`,
		`a\b`:    `'a\\b'`,
		`a\'b`:   `'a\\\'b'`,
		"a\x00b": `'a\0b'`,
	}
	for in, want := range cases {
		if got := quoteSQLString(in); got != want {
			t.Errorf("quoteSQLString(%q) = %s，期望 %s", in, got, want)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	ok := []string{"12345678", strings.Repeat("a", 128), "带中文的密码也行"}
	for _, p := range ok {
		if err := ValidatePassword(p); err != nil {
			t.Errorf("ValidatePassword(%q) 不该报错: %v", p, err)
		}
	}
	bad := []string{"", "short", strings.Repeat("a", 129), "has\nnewline", "has\x00nul"}
	for _, p := range bad {
		if err := ValidatePassword(p); err == nil {
			t.Errorf("ValidatePassword(%q) 应该报错", p)
		}
	}
}

func TestGeneratePasswordAvoidsAmbiguousChars(t *testing.T) {
	// 这个密码用户要抄进 Navicat 或配置文件，0/O/l/1/I 必须不出现
	seen := map[rune]bool{}
	for i := 0; i < 50; i++ {
		p, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != passwordLength {
			t.Fatalf("长度 = %d，期望 %d", len(p), passwordLength)
		}
		if err := ValidatePassword(p); err != nil {
			t.Fatalf("生成的密码通不过自己的校验: %v", err)
		}
		for _, r := range p {
			seen[r] = true
		}
	}
	for _, r := range "0Ol1I" {
		if seen[r] {
			t.Errorf("生成的密码里出现了容易看错的字符 %q", r)
		}
	}
	if len(seen) < 20 {
		t.Errorf("只用到了 %d 种字符，随机性可疑", len(seen))
	}
}

// my.cnf 里少一项就可能让 mariadbd 在沙箱里起不来，而且是"启动即退"
// 这种最难查的形式。这里把必须有的几项钉死。
func TestWriteConfig(t *testing.T) {
	dir := t.TempDir()
	p := &Paths{
		Install: dir, Data: filepath.Join(dir, "data"),
		Log: filepath.Join(dir, "log"), Cache: filepath.Join(dir, "cache"),
		Base: filepath.Join(dir, "mariadb"),
	}
	for _, d := range []string{p.Data, p.Log, p.Cache} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{path: filepath.Join(p.Data, "launcher.json"),
		data: ConfigData{Port: 3307}}
	srv := NewServer(p, cfg, filepath.Join(dir, "dbdata"))

	if err := srv.writeConfig(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(srv.cnfPath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)

	must := []string{
		"[mariadbd]",
		"port                    = 3307",
		"bind-address            = 127.0.0.1", // 默认不开局域网
		// 沙箱里没有 /etc/hosts，不跳过反查的话建连会很慢甚至失败
		"skip-name-resolve",
		// 沙箱里没有 /tmp，tmpdir 必须显式指到我们自己的目录
		"tmpdir",
		"character-set-server    = utf8mb4",
	}
	for _, m := range must {
		if !strings.Contains(text, m) {
			t.Errorf("my.cnf 里缺少 %q\n---\n%s", m, text)
		}
	}

	// 打开局域网访问后必须真的换成 0.0.0.0
	if err := cfg.Update(func(d *ConfigData) error { d.AllowLAN = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := srv.writeConfig(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(srv.cnfPath())
	if !strings.Contains(string(b), "bind-address            = 0.0.0.0") {
		t.Error("开启局域网访问后 bind-address 没有变成 0.0.0.0")
	}
}
