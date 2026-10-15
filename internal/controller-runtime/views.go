package controllerruntime

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/rs/zerolog/log"
)

//go:embed templates/*.html
var templateFiles embed.FS

// views holds the page templates, parsed at startup so that a broken one
// stops the process.
var views = template.Must(template.ParseFS(templateFiles, "templates/*.html"))

// rootView is what the root page template renders.
type rootView struct {
	ListURL string
}

// deploymentsView is what the deployment table template renders.
type deploymentsView struct {
	DeploymentURL string
	Deployments   []deployment
}

// deployment is a row of the table.
type deployment struct {
	Namespace string
	Name      string
	Replicas  int32
}

// render writes the named template as the response body
func (a *api) render(c *gin.Context, status int, name string, data any) {
	buf := &bytes.Buffer{}
	if err := views.ExecuteTemplate(buf, name, data); err != nil {
		log.Error().Err(err).Str("template", name).Msg("cannot render template")
		c.JSON(http.StatusInternalServerError, httpError{Err: "cannot render page"})
		return
	}
	c.Data(status, "text/html; charset=utf-8", buf.Bytes())
}
