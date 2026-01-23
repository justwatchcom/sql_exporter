package u2m

import (
	"bytes"
	_ "embed"
	"html/template"
)

type SimplePage struct {
	Title   string
	Heading string
	Content string
	Action  ActionLink
	Code    string
}

type ActionLink struct {
	Label string
	Link  string
}

var (
	//go:embed templates/simple.html
	simpleHtmlPage string
)

func renderHTML(data SimplePage) (string, error) {
	var out bytes.Buffer
	tmpl, err := template.New("name").Parse(simpleHtmlPage)
	if err != nil {
		return "", err
	}
	err = tmpl.Execute(&out, data)
	return out.String(), err
}

func infoHTML(title, content string) string {
	data := SimplePage{
		Title:   "Authentication Success",
		Heading: title,
		Content: content,
	}
	out, _ := renderHTML(data)
	return out
}

func errorHTML(msg string) string {
	data := SimplePage{
		Title:   "Authentication Error",
		Heading: "Ooops!",
		Content: "Sorry, Databricks could not authenticate to your account due to some server errors. Please try it later.",
		Code:    msg,
	}
	out, _ := renderHTML(data)
	return out
}
