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
	Rows []deploymentRow
}

// deploymentRow is one row of the deployment table: which deployment it is,
// and the cell of its replicas column.  The cell is carried as it is so that a
// row of the table and a cell swapped into a row of its own render the same.
type deploymentRow struct {
	Namespace string
	Name      string
	Cell      replicasView
}

// replicasView is what the replicas cell template renders: the number the
// deployment asks for, the form that number is changed with, or the number
// again carrying the reason an update did not happen.
type replicasView struct {
	// ReplicasURL is where the cell is asked for as the table shows it, and
	// where an edited number is put.
	ReplicasURL string
	// EditURL is where the cell is asked for as the form it is edited with.
	EditURL string
	// Replicas is the number of replicas the deployment asks for.
	Replicas int32
	// Limit is the largest number of replicas the service accepts, which the
	// form says so that the browser refuses the rest as well.
	Limit int32
	// Editing tells whether the cell is the form rather than the number.
	Editing bool
	// Message is why the last update did not happen, when it did not.
	Message string
}

// errorView is what the replicas error template renders, which is all that can
// be said of a row the service could not even draw.
type errorView struct {
	Message string
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
