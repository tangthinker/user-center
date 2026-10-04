package mail

// 嵌入一份 IANA 时区数据库。
//
// 为什么值得这几百 KB：本库要把时间渲染成"收件人当地的钟点"，而最容易踩坑的部署
// 恰恰是最小化容器（scratch / distroless / 精简版 alpine）——那种环境里没有
// /usr/share/zoneinfo，于是 time.LoadLocation("Asia/Shanghai") 直接失败，
// 宿主只能在"启动就报错"和"退回 UTC 显示（也就是这个时间不对的 bug）"之间二选一。
//
// 系统存在时区库时仍以系统为准，这里只是兜底（见 time/tzdata 的说明）。
import _ "time/tzdata"
