package main

import (
	"bufio"
	"strings"
	"testing"
)

func read(t *testing.T, wire string) (Value, error) {
	t.Helper()
	return readValue(bufio.NewReader(strings.NewReader(wire)))
}

func TestReadValueBasicTypes(t *testing.T) {
	t.Run("简单字符串", func(t *testing.T) {
		v, err := read(t, "+OK\r\n")
		if err != nil || v.Str != "OK" {
			t.Fatalf("got %+v, %v", v, err)
		}
	})
	t.Run("整数", func(t *testing.T) {
		v, err := read(t, ":42\r\n")
		if err != nil || v.Int != 42 {
			t.Fatalf("got %+v, %v", v, err)
		}
		if v.String() != "42" {
			t.Errorf("String() = %q", v.String())
		}
	})
	t.Run("负整数", func(t *testing.T) {
		v, _ := read(t, ":-1\r\n")
		if v.Int != -1 {
			t.Fatalf("got %+v", v)
		}
	})
	t.Run("批量字符串", func(t *testing.T) {
		v, err := read(t, "$5\r\nhello\r\n")
		if err != nil || v.Str != "hello" {
			t.Fatalf("got %+v, %v", v, err)
		}
	})
	t.Run("空批量字符串", func(t *testing.T) {
		v, err := read(t, "$0\r\n\r\n")
		if err != nil || v.Str != "" || v.Nil {
			t.Fatalf("got %+v, %v", v, err)
		}
	})
	t.Run("nil", func(t *testing.T) {
		v, err := read(t, "$-1\r\n")
		if err != nil || !v.Nil {
			t.Fatalf("got %+v, %v", v, err)
		}
		if v.String() != "(nil)" {
			t.Errorf("String() = %q", v.String())
		}
	})
	// 二进制安全：值里带 \r\n 是完全合法的，长度前缀说了算。
	// 按行读会把它截断，而且不会报错 —— 存进去的数据从此就是错的。
	t.Run("值里带换行", func(t *testing.T) {
		v, err := read(t, "$7\r\na\r\nb\r\nc\r\n")
		if err != nil {
			t.Fatal(err)
		}
		if v.Str != "a\r\nb\r\nc" {
			t.Fatalf("二进制没保住：%q", v.Str)
		}
	})
	t.Run("错误", func(t *testing.T) {
		_, err := read(t, "-ERR unknown command 'FOO'\r\n")
		if err == nil || !isRedisError(err) {
			t.Fatalf("应当是 RedisError，got %v", err)
		}
		if err.Error() != "ERR unknown command 'FOO'" {
			t.Errorf("消息不对：%q", err.Error())
		}
	})
}

func TestReadValueArrays(t *testing.T) {
	v, err := read(t, "*3\r\n$3\r\nfoo\r\n:7\r\n$-1\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Arr) != 3 {
		t.Fatalf("元素数 %d", len(v.Arr))
	}
	if v.Arr[0].Str != "foo" || v.Arr[1].Int != 7 || !v.Arr[2].Nil {
		t.Fatalf("内容不对：%+v", v.Arr)
	}

	t.Run("嵌套", func(t *testing.T) {
		v, err := read(t, "*2\r\n*2\r\n:1\r\n:2\r\n$2\r\nhi\r\n")
		if err != nil {
			t.Fatal(err)
		}
		if len(v.Arr) != 2 || len(v.Arr[0].Arr) != 2 {
			t.Fatalf("嵌套解析错了：%+v", v)
		}
	})

	// 数组【元素】里的错误是合法内容（EXEC 的结果就长这样）。
	// 让它中断整个数组的读取，会在连接里留下没读完的字节 ——
	// 之后每一条命令拿到的都是上一条的残骸，症状极其诡异。
	t.Run("元素里的错误不能中断整个数组", func(t *testing.T) {
		v, err := read(t, "*2\r\n-ERR wrong type\r\n$2\r\nok\r\n")
		if err != nil {
			t.Fatalf("不该整体失败：%v", err)
		}
		if len(v.Arr) != 2 {
			t.Fatalf("元素数 %d，应为 2", len(v.Arr))
		}
		if v.Arr[0].Type != '-' || v.Arr[1].Str != "ok" {
			t.Fatalf("内容不对：%+v", v.Arr)
		}
	})

	t.Run("空数组和 nil 数组", func(t *testing.T) {
		v, _ := read(t, "*0\r\n")
		if v.String() != "(empty array)" {
			t.Errorf("空数组 String() = %q", v.String())
		}
		v, _ = read(t, "*-1\r\n")
		if !v.Nil {
			t.Errorf("*-1 应当是 nil")
		}
	})
}

// 回复大小必须封顶：管理页上的「运行命令」能发任意命令，
// 一条 LRANGE biglist 0 -1 就能让服务端回几个 G，管理壳跟着 OOM 之后
// 用户连"停止"按钮都点不到了。
func TestReadValueRefusesHugeReplies(t *testing.T) {
	_, err := read(t, "$999999999\r\n")
	if err == nil || !strings.Contains(err.Error(), "太大") {
		t.Fatalf("超大批量回复应当被拒，got %v", err)
	}
	_, err = read(t, "*999999999\r\n")
	if err == nil || !strings.Contains(err.Error(), "太多") {
		t.Fatalf("超长数组应当被拒，got %v", err)
	}
}

func TestWriteCommand(t *testing.T) {
	var sb strings.Builder
	if err := writeCommand(&sb, []string{"SET", "k", "v v"}); err != nil {
		t.Fatal(err)
	}
	want := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$3\r\nv v\r\n"
	if sb.String() != want {
		t.Fatalf("got %q\nwant %q", sb.String(), want)
	}
}

// 【不能简单按空格 split】：SET greeting "hello world" 会被拆成四段，
// 存进去的值变成 "hello —— 而且不会报任何错，用户是过一阵子发现数据不对才回来找的。
func TestSplitArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`GET foo`, []string{"GET", "foo"}},
		{`  SET   a   b  `, []string{"SET", "a", "b"}},
		{`SET greeting "hello world"`, []string{"SET", "greeting", "hello world"}},
		{`SET k 'single quoted'`, []string{"SET", "k", "single quoted"}},
		{`SET k "with \"quote\""`, []string{"SET", "k", `with "quote"`}},
		{`SET k "line\nbreak"`, []string{"SET", "k", "line\nbreak"}},
		{`SET k "\x41\x42"`, []string{"SET", "k", "AB"}},
		{`SET k ""`, []string{"SET", "k", ""}},
		{``, nil},
		{`   `, nil},
	}
	for _, c := range cases {
		got, err := SplitArgs(c.in)
		if err != nil {
			t.Errorf("SplitArgs(%q) 报错: %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("SplitArgs(%q) = %q, want %q", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("SplitArgs(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestSplitArgsErrors(t *testing.T) {
	for _, in := range []string{`SET k "unterminated`, `SET k 'unterminated`, `SET "a"b`} {
		if _, err := SplitArgs(in); err == nil {
			t.Errorf("SplitArgs(%q) 应当报错", in)
		}
	}
}

func TestParseInfo(t *testing.T) {
	raw := "# Server\r\nredis_version:8.10.0\r\nuptime_in_days:3\r\n\r\n" +
		"# Keyspace\r\ndb0:keys=12,expires=3,avg_ttl=0\r\n"
	info := ParseInfo(raw)
	if info["redis_version"] != "8.10.0" {
		t.Fatalf("version = %q", info["redis_version"])
	}
	if info["uptime_in_days"] != "3" {
		t.Fatalf("uptime = %q", info["uptime_in_days"])
	}
	if _, ok := info["# Server"]; ok {
		t.Error("注释行不该被当成键")
	}
}

func TestParseKeyspaceSortsNumerically(t *testing.T) {
	info := map[string]string{
		"db10":          "keys=1,expires=0,avg_ttl=0",
		"db2":           "keys=5,expires=2,avg_ttl=0",
		"db0":           "keys=12,expires=3,avg_ttl=0",
		"redis_version": "8.10.0",
	}
	ks := ParseKeyspace(info)
	if len(ks) != 3 {
		t.Fatalf("条目数 %d，应为 3（redis_version 不该被算进来）", len(ks))
	}
	// 【按数字排，不能按字符串】：字符串序会把 db10 排在 db2 前面
	want := []string{"db0", "db2", "db10"}
	for i, e := range ks {
		if e.DB != want[i] {
			t.Fatalf("排序不对：%v", []string{ks[0].DB, ks[1].DB, ks[2].DB})
		}
	}
	if ks[0].Keys != 12 || ks[0].Expires != 3 {
		t.Errorf("db0 解析错了：%+v", ks[0])
	}
}
