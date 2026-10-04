// Package device 把 User-Agent 解析成"这台设备是什么"的可展示信息。
//
// 设计取舍（docs/auth-redesign.md §5.4）：
//   - 本库**不保存原始 UA**（只保存不可逆摘要），因此设备信息必须在签发会话时
//     解析成结构化字段落库：类型 / 型号 / 系统 / 浏览器；
//   - 除展示字段外，还产出**不含版本号**的指纹原像（Key），由调用方加盐哈希后
//     作为 device_id 落库：同一个客户端重复登录必须归并成"一台设备"，而
//     浏览器/系统升级**不能**凭空多出一台设备；
//   - 型号来自 UA，天然是尽力而为：iPhone 只知道是 iPhone、Android 通常是机型代号
//     （SM-G991B / Pixel 7）、桌面端只知道平台。UA 缺失或裁剪（如 Chromium 的
//     "Android 10; K"）时退化为平台名，绝不编造。
package device

import (
	"regexp"
	"strings"
)

// 设备类型。取值会落库，改动等于破坏既有数据，只允许追加。
const (
	TypeDesktop = "desktop"
	TypeMobile  = "mobile"
	TypeTablet  = "tablet"
	TypeBot     = "bot"
	TypeUnknown = "unknown"
)

// Info 是一台设备的可展示描述。
type Info struct {
	// Type 见上面的常量。
	Type string
	// Model 是设备型号的尽力而为值，如 "iPhone" / "Pixel 7" / "Mac"。
	Model string
	// OS 是系统与版本，如 "iOS 17.5"；无法识别时为空串。
	OS string
	// Browser 是浏览器与版本，如 "Chrome 126"；无法识别时为空串。
	Browser string

	// key 是指纹原像（不含版本号），只由 Parse 填充。
	key string
}

// KeyUnknown 是"什么都没解析出来"的指纹原像。
//
// 调用方据此决定不落库 device_id（留空）：这样"UA 认不出来"的新会话与
// 旧版本留下的 NULL device_id 会归成同一台"未知设备"。
const KeyUnknown = "unknown"

// Key 返回设备指纹原像。调用方必须**加盐哈希**后再落库：
// 直接存原像等于把"可跨库比对的设备标识"写进磁盘。
func (i Info) Key() string {
	if i.key == "" {
		return KeyUnknown
	}
	return i.key
}

var (
	reIOSVersion    = regexp.MustCompile(`os (\d+)[._](\d+)`)
	reAndroid       = regexp.MustCompile(`android\s*([0-9]+(?:\.[0-9]+)?)`)
	reWindowsNT     = regexp.MustCompile(`windows nt\s*([0-9]+\.[0-9]+)`)
	reMacOSVersion  = regexp.MustCompile(`mac os x\s*(\d+)[._](\d+)`)
	reVersionNumber = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)*`)
	reLocaleSegment = regexp.MustCompile(`^[a-z]{2}(-[a-z]{2,4})?$`)
	reHarmony       = regexp.MustCompile(`harmonyos|hmos`)
	// 通用爬虫标记必须带边界：Cubot 是手机品牌，不能因为含 "bot" 就被判成爬虫。
	reGenericBot = regexp.MustCompile(`(^|[^a-z])(bot|spider|crawler)([/ ;).+_-]|$)`)
)

// botRules 命中即判定为"非人类客户端"，并给出展示名。
//
// 必须排在浏览器规则之前：HeadlessChrome、okhttp、curl 的 UA 里都带着
// Chrome/ Safari/ 之类的子串，先匹配浏览器会把爬虫显示成"Chrome"。
var botRules = []struct{ token, label string }{
	{"googlebot", "Googlebot"},
	{"bingbot", "Bingbot"},
	{"baiduspider", "Baiduspider"},
	{"yandexbot", "YandexBot"},
	{"duckduckbot", "DuckDuckBot"},
	{"applebot", "Applebot"},
	{"twitterbot", "Twitterbot"},
	{"telegrambot", "TelegramBot"},
	{"slackbot", "Slackbot"},
	{"discordbot", "Discordbot"},
	{"facebookexternalhit", "Facebook"},
	{"bytespider", "Bytespider"},
	{"petalbot", "PetalBot"},
	{"semrushbot", "SemrushBot"},
	{"ahrefsbot", "AhrefsBot"},
	{"mj12bot", "MJ12bot"},
	{"dotbot", "DotBot"},
	{"sogou web spider", "Sogou Spider"},
	{"curl/", "curl"},
	{"wget", "Wget"},
	{"python-requests", "python-requests"},
	{"python-urllib", "python-urllib"},
	{"go-http-client", "Go http client"},
	{"okhttp", "OkHttp"},
	{"postmanruntime", "Postman"},
	{"java/", "Java"},
	{"libwww", "libwww-perl"},
	{"scrapy", "Scrapy"},
	{"headlesschrome", "Headless Chrome"},
	{"phantomjs", "PhantomJS"},
	{"axios/", "axios"},
	{"node-fetch", "node-fetch"},
	{"httpclient", "HTTP client"},
}

// browserRules 按顺序匹配：**越具体的必须排在越前面**（Edge 的 UA 里同时含
// Chrome/ 与 Safari/，Chrome 的 UA 里同时含 Safari/）。
//
// majorOnly 表示"只有主版本号有意义"：Chromium 系的 UA 是
// "126.0.2592.87" 这种补零 + 构建号写法，把 ".0" 显示出来纯属噪声；
// Safari / Firefox / 微信等的次版本号是有信息量的，必须保留。
var browserRules = []struct {
	family    string
	token     string // UA 中的标记（小写）
	label     string // 展示名
	majorOnly bool
}{
	{"edge", "edg/", "Edge", true},
	{"edge", "edga/", "Edge", true},
	{"edge", "edgios/", "Edge", true},
	{"opera", "opr/", "Opera", true},
	{"opera", "opera/", "Opera", false},
	{"samsung-internet", "samsungbrowser/", "Samsung Internet", true},
	{"yandex", "yabrowser/", "Yandex", true},
	{"vivaldi", "vivaldi/", "Vivaldi", true},
	{"wechat", "micromessenger/", "WeChat", false},
	{"qqbrowser", "qqbrowser/", "QQ Browser", false},
	{"uc", "ucbrowser/", "UC Browser", false},
	{"alipay", "alipayclient/", "Alipay", false},
	{"dingtalk", "dingtalk/", "DingTalk", false},
	{"firefox", "fxios/", "Firefox", false},
	{"firefox", "firefox/", "Firefox", false},
	{"chrome", "crios/", "Chrome", true},
	{"chrome", "chromium/", "Chromium", true},
	{"chrome", "chrome/", "Chrome", true},
	{"safari", "version/", "Safari", false},
	{"ie", "msie ", "Internet Explorer", true},
	{"ie", "trident/", "Internet Explorer", true},
}

// Parse 解析 UA。空串与无法识别的 UA 一律返回 Type=unknown 而不是报错：
// 设备信息是"锦上添花"，绝不能因为它挡住登录。
func Parse(ua string) Info {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return parsed{deviceType: TypeUnknown}.info()
	}
	raw := strings.ToLower(ua)

	if name := detectBot(raw); name != "" {
		// 爬虫/脚本没有"型号"与"浏览器"可言：客户端名即型号，系统留空。
		family := "bot:" + strings.ToLower(name)
		p := parsed{deviceType: TypeBot, model: name, browserFamily: family}
		return p.info()
	}

	p := parsed{}
	p.deviceType, p.model, p.os, p.osFamily = detectPlatform(ua, raw)
	p.browser, p.browserFamily = detectBrowser(raw)
	return p.info()
}

// parsed 是解析中间态：展示字段与指纹用的"族"分开存放。
type parsed struct {
	deviceType    string
	model         string
	os            string
	osFamily      string
	browser       string
	browserFamily string
}

func (p parsed) info() Info {
	if p.deviceType == "" {
		p.deviceType = TypeUnknown
	}
	if p.model == "" {
		p.model = fallbackModel(p.deviceType)
	}
	if p.osFamily == "" {
		p.osFamily = KeyUnknown
	}
	if p.browserFamily == "" {
		p.browserFamily = KeyUnknown
	}
	info := Info{
		Type:    p.deviceType,
		Model:   p.model,
		OS:      p.os,
		Browser: p.browser,
	}
	// 完全认不出来时给出统一的"未知"指纹：UA 为空、UA 不认识、以及旧版本留下的
	// NULL device_id 必须归成同一台"未知设备"，否则同一类东西会被拆成好几个。
	if p.deviceType == TypeUnknown && p.osFamily == KeyUnknown && p.browserFamily == KeyUnknown {
		info.key = KeyUnknown
		return info
	}
	// 指纹只由"族"构成：带上版本号会让浏览器/系统升级变成一台新设备。
	info.key = strings.Join([]string{
		p.deviceType, strings.ToLower(p.model), p.osFamily, p.browserFamily,
	}, "|")
	return info
}

func detectBot(raw string) string {
	for _, rule := range botRules {
		if strings.Contains(raw, rule.token) {
			return rule.label
		}
	}
	if reGenericBot.MatchString(raw) {
		return "Bot"
	}
	return ""
}

// detectPlatform 返回 (设备类型, 型号, 系统展示名, 系统族)。
func detectPlatform(ua, raw string) (deviceType, model, osName, osFamily string) {
	switch {
	case strings.Contains(raw, "ipod"):
		return TypeMobile, "iPod touch", iosVersion(raw, "iOS"), "ios"

	case strings.Contains(raw, "iphone"):
		return TypeMobile, "iPhone", iosVersion(raw, "iOS"), "ios"

	case strings.Contains(raw, "ipad"):
		return TypeTablet, "iPad", iosVersion(raw, "iPadOS"), "ios"

	case strings.Contains(raw, "android"):
		version := ""
		if m := reAndroid.FindStringSubmatch(raw); m != nil {
			version = m[1]
		}
		osName, osFamily = joinVersion("Android", version), "android"
		if reHarmony.MatchString(raw) {
			osName, osFamily = "HarmonyOS", "harmonyos"
		}
		model = androidModel(ua)
		if strings.Contains(raw, "mobile") {
			return TypeMobile, model, osName, osFamily
		}
		// Android 平板 / 电纸书：UA 里没有 Mobile
		return TypeTablet, model, osName, osFamily

	case strings.Contains(raw, "windows phone"):
		return TypeMobile, "Windows Phone", "Windows Phone", "windows"

	case strings.Contains(raw, "windows nt"):
		version := ""
		if m := reWindowsNT.FindStringSubmatch(raw); m != nil {
			version = m[1]
		}
		return TypeDesktop, "Windows PC", windowsName(version), "windows"

	case strings.Contains(raw, "cros"):
		return TypeDesktop, "Chromebook", "ChromeOS", "chromeos"

	case strings.Contains(raw, "mac os x"), strings.Contains(raw, "macintosh"):
		// iPadOS 13+ 的桌面模式 UA 是 "Macintosh; Intel Mac OS X 10_15" + "Mobile/15E148"
		if strings.Contains(raw, "mobile/") {
			return TypeTablet, "iPad", iosVersion(raw, "iPadOS"), "ios"
		}
		return TypeDesktop, "Mac", macOSName(raw), "macos"

	case strings.Contains(raw, "x11"), strings.Contains(raw, "linux"):
		return TypeDesktop, "Linux PC", "Linux", "linux"
	}
	return TypeUnknown, "", "", KeyUnknown
}

func iosVersion(raw, prefix string) string {
	m := reIOSVersion.FindStringSubmatch(raw)
	if m == nil {
		return prefix
	}
	return joinVersion(prefix, pairVersion(m[1], m[2]))
}

// macOSName 处理 Safari 的"版本冻结"：Safari 在 macOS 11+ 上仍然上报
// "Mac OS X 10_15_7"，把 10.15 当成真实版本会显示成老系统，因此只报 "macOS"。
func macOSName(raw string) string {
	m := reMacOSVersion.FindStringSubmatch(raw)
	if m == nil {
		return "macOS"
	}
	if m[1] == "10" && m[2] == "15" {
		return "macOS"
	}
	return joinVersion("macOS", pairVersion(m[1], m[2]))
}

func windowsName(ntVersion string) string {
	switch ntVersion {
	case "10.0":
		// Windows 10 与 11 共用 NT 10.0，UA 无法区分。
		return "Windows 10/11"
	case "6.3":
		return "Windows 8.1"
	case "6.2":
		return "Windows 8"
	case "6.1":
		return "Windows 7"
	case "6.0":
		return "Windows Vista"
	case "5.1", "5.2":
		return "Windows XP"
	case "":
		return "Windows"
	}
	return joinVersion("Windows NT", ntVersion)
}

// androidModel 从 UA 的括号注释里取机型：
//
//	Mozilla/5.0 (Linux; Android 13; Pixel 7 Build/TQ3A...) AppleWebKit/537.36
//	Mozilla/5.0 (Linux; Android 13; zh-CN; SM-S9180) AppleWebKit/537.36
//
// 机型前可能是语言标签（必须跳过），机型后可能跟 "Build/..."（必须截断）。
func androidModel(ua string) string {
	open := strings.Index(ua, "(")
	closing := strings.Index(ua, ")")
	if open < 0 || closing <= open {
		return ""
	}
	segments := strings.Split(ua[open+1:closing], ";")
	androidAt := -1
	for i, seg := range segments {
		if strings.Contains(strings.ToLower(seg), "android") {
			androidAt = i
			break
		}
	}
	if androidAt < 0 {
		return ""
	}
	for _, seg := range segments[androidAt+1:] {
		candidate := strings.TrimSpace(seg)
		if candidate == "" || reLocaleSegment.MatchString(strings.ToLower(candidate)) {
			continue
		}
		if at := strings.Index(candidate, " Build"); at >= 0 {
			candidate = strings.TrimSpace(candidate[:at])
		}
		// Chromium 的 UA 削减方案把机型替换成占位符，这些不是机型。
		switch strings.ToLower(candidate) {
		case "k", "wv", "mobile", "build":
			return ""
		}
		return candidate
	}
	return ""
}

func detectBrowser(raw string) (label, family string) {
	for _, rule := range browserRules {
		i := strings.Index(raw, rule.token)
		if i < 0 {
			continue
		}
		return joinVersion(rule.label, versionAfter(raw[i+len(rule.token):], rule.majorOnly)), rule.family
	}
	return "", ""
}

// versionAfter 取出紧随标记的版本号（"safari/605.1.15" → "605.1"）。
//
// majorOnly 时只保留主版本号；否则保留"主.次"两位。
func versionAfter(s string, majorOnly bool) string {
	token := reVersionNumber.FindString(s)
	if token == "" {
		return ""
	}
	parts := strings.Split(token, ".")
	if majorOnly {
		return parts[0]
	}
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return strings.Join(parts, ".")
}

// pairVersion 组合主次版本号，"17.0" 收敛成 "17"。
func pairVersion(major, minor string) string {
	if minor == "0" {
		return major
	}
	return major + "." + minor
}

func joinVersion(name, version string) string {
	if version == "" {
		return name
	}
	return name + " " + version
}

// fallbackModel 在拿不到型号时至少给出平台名，避免界面上出现空单元格。
func fallbackModel(deviceType string) string {
	switch deviceType {
	case TypeDesktop:
		return "桌面设备"
	case TypeMobile:
		return "手机"
	case TypeTablet:
		return "平板"
	case TypeBot:
		return "机器人"
	}
	return "未知设备"
}
