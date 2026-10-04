package device_test

import (
	"testing"

	"github.com/tangthinker/user-center/v2/internal/device"
)

// 全部使用真实世界的 UA（浏览器更新频繁，这里只断言"族 + 主版本"级别的结论）。
func TestParseRecognizesCommonClients(t *testing.T) {
	cases := []struct {
		name string
		ua   string
		want device.Info
	}{
		{
			name: "iPhone Safari",
			ua:   `Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1`,
			want: device.Info{Type: device.TypeMobile, Model: "iPhone", OS: "iOS 17.5", Browser: "Safari 17.5"},
		},
		{
			name: "iPhone Chrome",
			ua:   `Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/126.0.6478.54 Mobile/15E148 Safari/604.1`,
			want: device.Info{Type: device.TypeMobile, Model: "iPhone", OS: "iOS 17.5", Browser: "Chrome 126"},
		},
		{
			name: "iPad",
			ua:   `Mozilla/5.0 (iPad; CPU OS 16_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.6 Mobile/15E148 Safari/604.1`,
			want: device.Info{Type: device.TypeTablet, Model: "iPad", OS: "iPadOS 16.6", Browser: "Safari 16.6"},
		},
		{
			// iPadOS 13+ 的"桌面模式"UA：伪装成 Macintosh，靠 Mobile/ 认出来
			name: "iPadOS desktop mode",
			ua:   `Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1`,
			// 桌面模式 UA 里只有被冻结的 "Mac OS X 10_15"，拿不到真实 iPadOS 版本
			want: device.Info{Type: device.TypeTablet, Model: "iPad", OS: "iPadOS", Browser: "Safari 17.5"},
		},
		{
			name: "Android Chrome",
			ua:   `Mozilla/5.0 (Linux; Android 13; Pixel 7 Build/TQ3A.230805.001) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/115.0.0.0 Mobile Safari/537.36`,
			want: device.Info{Type: device.TypeMobile, Model: "Pixel 7", OS: "Android 13", Browser: "Chrome 115"},
		},
		{
			// 语言标签必须在机型之前被跳过
			name: "Android with locale segment",
			ua:   `Mozilla/5.0 (Linux; U; Android 13; zh-CN; 2201123C Build/TKQ1.221114.001) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/112.0.0.0 Mobile Safari/537.36`,
			want: device.Info{Type: device.TypeMobile, Model: "2201123C", OS: "Android 13", Browser: "Chrome 112"},
		},
		{
			// Chromium 的 UA 削减方案把机型替换成占位符 "K"：宁可不报，不能瞎报
			name: "Android reduced UA",
			ua:   `Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36`,
			want: device.Info{Type: device.TypeMobile, Model: "手机", OS: "Android 10", Browser: "Chrome 126"},
		},
		{
			name: "WeChat Android",
			ua:   `Mozilla/5.0 (Linux; Android 12; SM-G991B Build/SP1A.210812.016; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/107.0.0.0 Mobile Safari/537.36 MicroMessenger/8.0.49(0x2800313D) WeChat/arm64 NetType/WIFI Language/zh_CN`,
			want: device.Info{Type: device.TypeMobile, Model: "SM-G991B", OS: "Android 12", Browser: "WeChat 8.0"},
		},
		{
			name: "Android tablet without Mobile token",
			ua:   `Mozilla/5.0 (Linux; Android 13; SM-X700) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/23.0 Chrome/115.0.0.0 Safari/537.36`,
			want: device.Info{Type: device.TypeTablet, Model: "SM-X700", OS: "Android 13", Browser: "Samsung Internet 23"},
		},
		{
			name: "Windows Chrome",
			ua:   `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36`,
			want: device.Info{Type: device.TypeDesktop, Model: "Windows PC", OS: "Windows 10/11", Browser: "Chrome 126"},
		},
		{
			name: "Windows Edge",
			ua:   `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.2592.87`,
			want: device.Info{Type: device.TypeDesktop, Model: "Windows PC", OS: "Windows 10/11", Browser: "Edge 126"},
		},
		{
			// Safari 在 macOS 11+ 仍上报 10_15_7：显示成 macOS 10.15 就是错的
			name: "macOS Safari version freeze",
			ua:   `Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15`,
			want: device.Info{Type: device.TypeDesktop, Model: "Mac", OS: "macOS", Browser: "Safari 17.5"},
		},
		{
			name: "Linux Firefox",
			ua:   `Mozilla/5.0 (X11; Linux x86_64; rv:127.0) Gecko/20100101 Firefox/127.0`,
			want: device.Info{Type: device.TypeDesktop, Model: "Linux PC", OS: "Linux", Browser: "Firefox 127.0"},
		},
		{
			name: "ChromeOS",
			ua:   `Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36`,
			want: device.Info{Type: device.TypeDesktop, Model: "Chromebook", OS: "ChromeOS", Browser: "Chrome 126"},
		},
		{
			// 回归：CUBOT 是手机品牌，不能因为含 "bot" 被判成爬虫
			name: "Cubot phone is not a bot",
			ua:   `Mozilla/5.0 (Linux; Android 9; CUBOT_X20) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/70.0.3538.80 Mobile Safari/537.36`,
			want: device.Info{Type: device.TypeMobile, Model: "CUBOT_X20", OS: "Android 9", Browser: "Chrome 70"},
		},
		{
			name: "curl",
			ua:   `curl/8.4.0`,
			want: device.Info{Type: device.TypeBot, Model: "curl"},
		},
		{
			// 爬虫 UA 里带着 Chrome/Safari 子串，必须按爬虫处理
			name: "HeadlessChrome",
			ua:   `Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/126.0.0.0 Safari/537.36`,
			want: device.Info{Type: device.TypeBot, Model: "Headless Chrome"},
		},
		{
			name: "Googlebot",
			ua:   `Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)`,
			want: device.Info{Type: device.TypeBot, Model: "Googlebot"},
		},
		{
			name: "empty UA",
			ua:   ``,
			want: device.Info{Type: device.TypeUnknown, Model: "未知设备"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := device.Parse(tc.ua)
			if got.Type != tc.want.Type {
				t.Errorf("Type = %q, want %q", got.Type, tc.want.Type)
			}
			if got.Model != tc.want.Model {
				t.Errorf("Model = %q, want %q", got.Model, tc.want.Model)
			}
			if got.OS != tc.want.OS {
				t.Errorf("OS = %q, want %q", got.OS, tc.want.OS)
			}
			if got.Browser != tc.want.Browser {
				t.Errorf("Browser = %q, want %q", got.Browser, tc.want.Browser)
			}
		})
	}
}

// 指纹必须忽略版本号：否则浏览器/系统一升级就"多出一台设备"。
func TestKeyIgnoresVersions(t *testing.T) {
	older := device.Parse(`Mozilla/5.0 (Linux; Android 13; Pixel 7 Build/X) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/115.0.0.0 Mobile Safari/537.36`)
	newer := device.Parse(`Mozilla/5.0 (Linux; Android 14; Pixel 7 Build/Y) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36`)
	if older.Key() != newer.Key() {
		t.Errorf("版本升级不该改变指纹：%q != %q", older.Key(), newer.Key())
	}

	same := device.Parse(`Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1`)
	patched := device.Parse(`Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1`)
	if same.Key() != patched.Key() {
		t.Errorf("系统小版本升级不该改变指纹：%q != %q", same.Key(), patched.Key())
	}
}

// 不同机型/不同浏览器必须区分开：否则"设备数"就是一锅粥。
func TestKeyDistinguishesDevices(t *testing.T) {
	pixel7 := device.Parse(`Mozilla/5.0 (Linux; Android 13; Pixel 7 Build/X) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/115.0.0.0 Mobile Safari/537.36`)
	pixel8 := device.Parse(`Mozilla/5.0 (Linux; Android 13; Pixel 8 Build/X) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/115.0.0.0 Mobile Safari/537.36`)
	if pixel7.Key() == pixel8.Key() {
		t.Error("不同机型必须得到不同指纹")
	}

	iphoneSafari := device.Parse(`Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1`)
	iphoneChrome := device.Parse(`Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/126.0.6478.54 Mobile/15E148 Safari/604.1`)
	if iphoneSafari.Key() == iphoneChrome.Key() {
		t.Error("同机型的不同浏览器必须得到不同指纹")
	}
}

// 零值 Info（例如从数据库读回来的旧会话）不能产生"空指纹"。
func TestZeroValueKeyIsStable(t *testing.T) {
	var zero device.Info
	if zero.Key() != "unknown" {
		t.Fatalf("zero key = %q, want unknown", zero.Key())
	}
}
