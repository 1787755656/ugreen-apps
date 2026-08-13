package main

import "testing"

// project.yaml 的 start_cmd 是端口的唯一来源，这里钉住解析行为——
// 解析坏了的表现是应用起在 8080（上游默认）而 project.yaml 声明的是别的端口，
// 应用中心会一直显示"未启动"，且日志里看不出原因。
func TestPortFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"等号写法", []string{"magicmail", "--port=23232"}, "23232"},
		{"空格写法", []string{"magicmail", "--port", "23232"}, "23232"},
		{"没有参数", []string{"magicmail"}, ""},
		{"别的参数", []string{"magicmail", "--verbose"}, ""},
		{"--port 在末尾没有值", []string{"magicmail", "--port"}, ""},
		{"程序名本身不参与匹配", []string{"--port=1"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := portFromArgs(c.args); got != c.want {
				t.Errorf("portFromArgs(%q) = %q, want %q", c.args, got, c.want)
			}
		})
	}
}
