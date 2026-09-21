package main

// 说明文字（caption）的 Markdown / HTML → MessageEntity 解析。
// 对应 Telethon send_file(parse_mode=...) 的常用子集：
//
//	**粗体** __斜体__ ~~删除线** ||剧透|| `代码` ```预格式``` [文字](链接)
//	<b> <i> <u> <s> <code> <pre> <a href> <tg-spoiler>
//
// 实体的 offset/length 必须按 UTF-16 编码单元计数（Telegram 协议要求）。
// 为控制复杂度，标记不嵌套（嵌套标记按字面量处理）——文件名类 caption 足够了。

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/tg"
)

// sanitizeUTF8 把非法 UTF-8 字节替换成 U+FFFD。
//
// 这不只是为了整洁，而是实体 offset/length 正确性的前提：
// 这两个值必须按 UTF-16 编码单元计数，而「各片段长度之和」只有在每段都是
// 合法 UTF-8 时才等于整体长度。非法字节会在拼接处重新组合成合法序列——
// 例如 `\xd2` 与 `\x87` 单独看都是坏字节，拼在一起却是合法字符 U+0487，
// 于是计数器比实际长度多算一格，算出来的实体就越界，整条消息被 Telegram 拒收。
//
// 模糊测试用输入 "000000\xd2**\x87**" 命中的就是这个（旧实现同样有问题：
// 它虽然在完整串上测 offset，但 length 仍然只测片段）。
//
// 净化之后再切分是安全的：Go 的 regexp 按 rune 匹配，合法 UTF-8 输入上
// 匹配边界必然落在 rune 边界，任何子匹配切片都是合法 UTF-8。
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// mdRe 把 pre/code/bold/italic/strike/spoiler 与链接合并成一个交替正则。
// 放在包级，避免每次调用重新编译（正则编译是这里最贵的操作）。
var mdRe = regexp.MustCompile("```([A-Za-z0-9_+-]*)\\n?([\\s\\S]*?)```" +
	"|\\*\\*([\\s\\S]+?)\\*\\*" +
	"|__([\\s\\S]+?)__" +
	"|~~([\\s\\S]+?)~~" +
	"|\\|\\|([\\s\\S]+?)\\|\\|" +
	"|`([^`\\n]+)`" +
	"|\\[([^\\]]+)\\]\\(([^)\\s]+)\\)")

// markdownEntities 解析 Markdown 标记，返回纯文本与实体列表。
//
// u16 维护「已产出的纯文本的 UTF-16 长度」，随 emit 增量累加。
// 早先的实现每次都调 utf16Len(out.String()) 重算整串，整体是 O(n²)；
// caption 通常很短所以没炸，但没理由留着。
func markdownEntities(text string) (string, []tg.MessageEntityClass) {
	text = sanitizeUTF8(text)
	var out strings.Builder
	var entities []tg.MessageEntityClass
	pos := 0 // 已消费的源串字节偏移
	u16 := 0 // 已产出纯文本的 UTF-16 长度

	emit := func(s string) {
		out.WriteString(s)
		u16 += utf16Len(s)
	}
	// add 先取 offset 再 emit，所以顺序不能颠倒
	add := func(content string, mk func(off, ln int) tg.MessageEntityClass) {
		off := u16
		emit(content)
		if ln := utf16Len(content); ln > 0 {
			entities = append(entities, mk(off, ln))
		}
	}

	for pos < len(text) {
		// FindStringSubmatchIndex 一次拿全部组的边界，
		// 不再用 FindStringSubmatch 把同一个正则重复跑一遍。
		loc := mdRe.FindStringSubmatchIndex(text[pos:])
		if loc == nil {
			emit(text[pos:])
			break
		}
		emit(text[pos : pos+loc[0]])

		// loc 的下标以 text[pos:] 为基准，切片时必须用同一个基准
		group := func(i int) string {
			s, e := loc[2*i], loc[2*i+1]
			if s < 0 || e < 0 {
				return ""
			}
			return text[pos+s : pos+e]
		}
		matched := text[pos+loc[0] : pos+loc[1]]

		switch {
		case strings.HasPrefix(matched, "```"):
			lang, code := group(1), group(2)
			off := u16
			emit(code)
			// 必须挡住空正文：` ``````` ` 这类输入（正则的 code 组是 * 而非 +）
			// 会产生 length=0 的实体，Telegram 会拒收整条消息。
			// 模糊测试用输入 "``````" 命中的就是这个分支。
			if ln := utf16Len(code); ln > 0 {
				entities = append(entities, &tg.MessageEntityPre{
					Offset: off, Length: ln, Language: lang,
				})
			}
		case strings.HasPrefix(matched, "**"):
			add(group(3), func(off, ln int) tg.MessageEntityClass {
				return &tg.MessageEntityBold{Offset: off, Length: ln}
			})
		case strings.HasPrefix(matched, "__"):
			add(group(4), func(off, ln int) tg.MessageEntityClass {
				return &tg.MessageEntityItalic{Offset: off, Length: ln}
			})
		case strings.HasPrefix(matched, "~~"):
			add(group(5), func(off, ln int) tg.MessageEntityClass {
				return &tg.MessageEntityStrike{Offset: off, Length: ln}
			})
		case strings.HasPrefix(matched, "||"):
			add(group(6), func(off, ln int) tg.MessageEntityClass {
				return &tg.MessageEntitySpoiler{Offset: off, Length: ln}
			})
		case strings.HasPrefix(matched, "`"):
			add(group(7), func(off, ln int) tg.MessageEntityClass {
				return &tg.MessageEntityCode{Offset: off, Length: ln}
			})
		default:
			label, link := group(8), group(9)
			off := u16
			emit(label)
			if ln := utf16Len(label); ln > 0 {
				entities = append(entities, &tg.MessageEntityTextURL{
					Offset: off, Length: ln, URL: link,
				})
			}
		}
		pos += loc[1]
	}
	return out.String(), entities
}

var htmlTagRe = regexp.MustCompile(
	`(?i)<(/?)(b|strong|i|em|u|ins|s|strike|del|code|pre|a|tg-spoiler)\b([^>]*)>`)

// 只支持 class / href 两个属性，预编译成两个常量正则，
// 而不是每次调用 attrValue 都 MustCompile 一遍。
var (
	htmlClassRe = regexp.MustCompile(`(?i)class\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	htmlHrefRe  = regexp.MustCompile(`(?i)href\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

func htmlUnescape(s string) string {
	r := strings.NewReplacer(
		"&lt;", "<", "&gt;", ">", "&amp;", "&",
		"&quot;", `"`, "&#39;", "'", "&#x27;", "'", "&nbsp;", " ",
	)
	return r.Replace(s)
}

// htmlEntities 解析受支持的 HTML 标签，返回纯文本与实体列表。标签不配对时宽容处理。
func htmlEntities(text string) (string, []tg.MessageEntityClass) {
	text = sanitizeUTF8(text)
	type openTag struct {
		kind  string
		start int
		attr  string
	}
	var out strings.Builder
	var entities []tg.MessageEntityClass
	var stack []openTag
	u16 := 0

	emit := func(s string) {
		out.WriteString(s)
		u16 += utf16Len(s)
	}
	pop := func(kind string) *openTag {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].kind == kind {
				t := stack[i]
				stack = append(stack[:i], stack[i+1:]...)
				return &t
			}
		}
		return nil
	}
	addEntity := func(kind string, start int, attr string) {
		length := u16 - start
		if length <= 0 {
			return
		}
		var e tg.MessageEntityClass
		switch kind {
		case "b", "strong":
			e = &tg.MessageEntityBold{Offset: start, Length: length}
		case "i", "em":
			e = &tg.MessageEntityItalic{Offset: start, Length: length}
		case "u", "ins":
			e = &tg.MessageEntityUnderline{Offset: start, Length: length}
		case "s", "strike", "del":
			e = &tg.MessageEntityStrike{Offset: start, Length: length}
		case "code":
			e = &tg.MessageEntityCode{Offset: start, Length: length}
		case "pre":
			lang := ""
			if cls := attrValue(attr, "class"); strings.HasPrefix(cls, "language-") {
				lang = strings.TrimPrefix(cls, "language-")
			}
			e = &tg.MessageEntityPre{Offset: start, Length: length, Language: lang}
		case "tg-spoiler":
			e = &tg.MessageEntitySpoiler{Offset: start, Length: length}
		case "a":
			e = &tg.MessageEntityTextURL{Offset: start, Length: length,
				URL: attrValue(attr, "href")}
		}
		if e != nil {
			entities = append(entities, e)
		}
	}

	pos := 0
	for pos < len(text) {
		loc := htmlTagRe.FindStringSubmatchIndex(text[pos:])
		if loc == nil {
			emit(htmlUnescape(text[pos:]))
			break
		}
		emit(htmlUnescape(text[pos : pos+loc[0]]))

		group := func(i int) string {
			s, e := loc[2*i], loc[2*i+1]
			if s < 0 || e < 0 {
				return ""
			}
			return text[pos+s : pos+e]
		}
		closing := group(1) == "/"
		tag := strings.ToLower(group(2))
		attrs := group(3)

		if closing {
			if t := pop(tag); t != nil {
				addEntity(tag, t.start, t.attr)
			}
		} else {
			stack = append(stack, openTag{kind: tag, start: u16, attr: attrs})
		}
		pos += loc[1]
	}
	for _, t := range stack { // 未闭合标签按到文末处理
		addEntity(t.kind, t.start, t.attr)
	}
	return out.String(), entities
}

func attrValue(attrs, name string) string {
	var re *regexp.Regexp
	switch strings.ToLower(name) {
	case "class":
		re = htmlClassRe
	case "href":
		re = htmlHrefRe
	default:
		return ""
	}
	m := re.FindStringSubmatch(attrs)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return htmlUnescape(m[1])
	}
	return htmlUnescape(m[2])
}

// buildCaption 按 parse_mode 生成 (纯文本, 实体)。
func buildCaption(text, parseMode string) (string, []tg.MessageEntityClass) {
	if text == "" || parseMode == "none" || parseMode == "" {
		return text, nil
	}
	if parseMode == "html" {
		return htmlEntities(text)
	}
	return markdownEntities(text) // 默认 md
}
