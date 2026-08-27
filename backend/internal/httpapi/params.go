package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// intQuery reads a bounded integer query parameter.
//
// An unparseable or out-of-range value is clamped rather than rejected. Query
// parameters are a display concern — a bad ?limit= should show a page, not an
// error — whereas a bad *filter* value is rejected, because silently ignoring a
// filter shows the operator data they did not ask for.
func intQuery(c *gin.Context, name string, def, min, max int) int {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// int64Query reads an int64 query parameter, used for the benchmark seed.
func int64Query(c *gin.Context, name string, def int64) int64 {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}

// floatQuery reads a float in [min,max].
func floatQuery(c *gin.Context, name string, def, min, max float64) float64 {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// caseSortFields are the orderings the store supports. Listed here so an
// unsupported value is a 400 naming the alternatives, rather than a silent
// fallback that makes the UI look like it ignored the click.
var caseSortFields = []string{"expected_recovery", "risk_score", "created_at", "revenue_at_risk"}

// parseCaseFilter builds the list filter from the query string (SRS 16.2).
//
// Enum values are validated against the domain vocabulary. A typo in ?status= is
// refused rather than treated as "no filter": an operator who filters for
// ESCALATED and is shown every case would read the result as "nothing is
// escalated".
func (s *Server) parseCaseFilter(c *gin.Context) (domain.CaseFilter, bool) {
	f := domain.CaseFilter{
		Limit:  intQuery(c, "limit", 25, 1, 200),
		Offset: intQuery(c, "offset", 0, 0, 100000),
	}
	details := map[string]string{}

	if v := strings.TrimSpace(c.Query("source_type")); v != "" {
		st := domain.SourceType(v)
		if !st.Valid() {
			details["source_type"] = "unknown source type " + quote(v) + "; expected one of " + joinSourceTypes()
		} else {
			f.SourceType = st
		}
	}
	if v := strings.TrimSpace(c.Query("segment")); v != "" {
		seg := domain.Segment(v)
		if !seg.Valid() {
			details["segment"] = "unknown segment " + quote(v) + "; expected one of " + joinSegments()
		} else {
			f.Segment = seg
		}
	}
	if v := strings.TrimSpace(c.Query("status")); v != "" {
		st := domain.CaseStatus(strings.ToUpper(v))
		if !st.Valid() {
			details["status"] = "unknown case status " + quote(v)
		} else {
			f.Status = st
		}
	}
	if v := strings.TrimSpace(c.Query("action_type")); v != "" {
		at := domain.ActionType(v)
		if !at.Valid() {
			details["action_type"] = "unknown action type " + quote(v)
		} else {
			f.ActionType = at
		}
	}
	if v := strings.TrimSpace(c.Query("mode")); v != "" {
		mode := domain.RunMode(v)
		if !mode.Valid() {
			details["mode"] = "unknown run mode " + quote(v)
		} else {
			f.Mode = mode
		}
	}
	if v := strings.TrimSpace(c.Query("sort_by")); v != "" {
		if !contains(caseSortFields, v) {
			details["sort_by"] = "unsupported sort field " + quote(v) + "; expected one of " + strings.Join(caseSortFields, ", ")
		} else {
			f.SortBy = v
		}
	}

	f.MinRisk = floatQuery(c, "min_risk", 0, 0, 1)
	// Search is free text and reaches the store as a bound parameter, never as
	// interpolated SQL. It is length-capped so a pathological pattern cannot turn
	// a list query into a scan of the whole table.
	f.Search = truncate(strings.TrimSpace(c.Query("search")), 120)

	if len(details) > 0 {
		failValidation(c, details)
		return f, false
	}
	return f, true
}

// pathID reads a required path parameter, rejecting the shapes that cannot be an
// identifier this system issued.
func pathID(c *gin.Context, name string) (string, bool) {
	v := strings.TrimSpace(c.Param(name))
	switch {
	case v == "":
		failWith(c, http.StatusBadRequest, "missing_id", "an identifier is required in the path")
		return "", false
	case len(v) > 64:
		// Store ids are short prefixed strings. A long value is a client bug or a
		// probe, and either way there is nothing to look up.
		failWith(c, http.StatusBadRequest, "invalid_id", "that identifier is not well formed")
		return "", false
	}
	return v, true
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func quote(s string) string { return "\"" + truncate(s, 40) + "\"" }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func joinSourceTypes() string {
	parts := make([]string, 0, len(domain.AllSourceTypes))
	for _, v := range domain.AllSourceTypes {
		parts = append(parts, string(v))
	}
	return strings.Join(parts, ", ")
}

func joinSegments() string {
	parts := make([]string, 0, len(domain.AllSegments))
	for _, v := range domain.AllSegments {
		parts = append(parts, string(v))
	}
	return strings.Join(parts, ", ")
}

func joinStrategies() string {
	parts := make([]string, 0, len(domain.AllStrategies))
	for _, v := range domain.AllStrategies {
		parts = append(parts, string(v))
	}
	return strings.Join(parts, ", ")
}

func itoa(n int) string { return strconv.Itoa(n) }

// percent renders a 0..1 ratio for display. One decimal place: the underlying
// numbers are counts of a few hundred cases, and more precision than that would
// imply a resolution the sample size does not have.
func percent(ratio float64) string {
	return strconv.FormatFloat(ratio*100, 'f', 1, 64) + "%"
}

// signedPercent keeps the sign visible, because an honest uplift figure has to be
// able to be negative (SRS AC-008).
func signedPercent(pct float64) string {
	s := strconv.FormatFloat(pct, 'f', 1, 64)
	if pct > 0 {
		s = "+" + s
	}
	return s + "%"
}
