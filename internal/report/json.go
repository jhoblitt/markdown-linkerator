package report

import "encoding/json"

// jsonReport is the stable wire shape emitted in json format. It is decoupled
// from model.Result so the output schema does not track internal struct
// changes (and so error values render as text rather than "{}").
type jsonReport struct {
	Total    int          `json:"total"`
	Alive    int          `json:"alive"`
	Dead     int          `json:"dead"`
	Ignored  int          `json:"ignored"`
	Errored  int          `json:"errored"`
	ExitCode int          `json:"exitCode"`
	Results  []jsonResult `json:"results"`
	Hosts    []jsonHost   `json:"hosts"`
}

type jsonResult struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	URL        string `json:"url"`
	State      string `json:"state"`
	StatusCode int    `json:"statusCode"`
	Detail     string `json:"detail"`
}

type jsonHost struct {
	Host        string  `json:"host"`
	Requests    int64   `json:"requests"`
	Retries     int64   `json:"retries"`
	N429        int64   `json:"n429"`
	ObservedRPS float64 `json:"observedRps"`
}

func (c *Collector) renderJSON(s Summary) {
	jr := jsonReport{
		Total:    s.Total,
		Alive:    s.Alive,
		Dead:     s.Dead,
		Ignored:  s.Ignored,
		Errored:  s.Errored,
		ExitCode: s.ExitCode,
		Results:  make([]jsonResult, 0, len(s.Results)),
		Hosts:    make([]jsonHost, 0, len(s.Hosts)),
	}
	for _, r := range s.Results {
		jr.Results = append(jr.Results, jsonResult{
			File:       r.Target.SourceFile,
			Line:       r.Target.Line,
			URL:        r.Target.URL,
			State:      r.State.String(),
			StatusCode: r.StatusCode,
			Detail:     detailText(r),
		})
	}
	for _, h := range s.Hosts {
		jr.Hosts = append(jr.Hosts, jsonHost{
			Host:        h.Host,
			Requests:    h.Requests,
			Retries:     h.Retries,
			N429:        h.N429,
			ObservedRPS: h.ObservedRPS,
		})
	}
	enc := json.NewEncoder(c.opts.Out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // keep URLs (& ? =) literal
	_ = enc.Encode(jr)
}
