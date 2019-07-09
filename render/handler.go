package render

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/lomik/graphite-clickhouse/config"
	"github.com/lomik/graphite-clickhouse/finder"
	"github.com/lomik/graphite-clickhouse/helper/clickhouse"
	"github.com/lomik/graphite-clickhouse/helper/point"
	"github.com/lomik/graphite-clickhouse/helper/rollup"
	"github.com/lomik/graphite-clickhouse/pkg/dry"
	"github.com/lomik/graphite-clickhouse/pkg/reverse"
	"github.com/lomik/graphite-clickhouse/pkg/scope"
	"github.com/lomik/graphite-clickhouse/pkg/where"

	graphitePickle "github.com/lomik/graphite-pickle"
)

type Handler struct {
	config     *config.Config
	carbonlink *graphitePickle.CarbonlinkClient
}

func NewHandler(config *config.Config) *Handler {
	h := &Handler{
		config: config,
	}

	if config.Carbonlink.Server != "" {
		h.carbonlink = graphitePickle.NewCarbonlinkClient(
			config.Carbonlink.Server,
			config.Carbonlink.Retries,
			config.Carbonlink.Threads,
			config.Carbonlink.ConnectTimeout.Value(),
			config.Carbonlink.QueryTimeout.Value(),
		)
	}
	return h
}

// returns callable result fetcher
func (h *Handler) queryCarbonlink(parentCtx context.Context, logger *zap.Logger, metrics []string) func() *point.Points {
	if h.carbonlink == nil {
		return func() *point.Points { return nil }
	}

	carbonlinkResponseChan := make(chan *point.Points, 1)

	fetchResult := func() *point.Points {
		result := <-carbonlinkResponseChan
		return result
	}

	go func() {
		ctx, cancel := context.WithTimeout(parentCtx, h.config.Carbonlink.TotalTimeout.Value())
		defer cancel()

		res, err := h.carbonlink.CacheQueryMulti(ctx, metrics)

		if err != nil {
			logger.Info("carbonlink failed", zap.Error(err))
		}

		result := point.NewPoints()

		if res != nil && len(res) > 0 {
			tm := uint32(time.Now().Unix())

			for metric, points := range res {
				metricID := result.MetricID(metric)
				for _, p := range points {
					result.AppendPoint(metricID, p.Value, uint32(p.Timestamp), tm)
				}
			}
		}

		carbonlinkResponseChan <- result
	}()

	return fetchResult
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := scope.Logger(r.Context())

	var prefix string
	var err error

	r.ParseMultipartForm(1024 * 1024)

	fromTimestamp, err := strconv.ParseInt(r.FormValue("from"), 10, 32)
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	untilTimestamp, err := strconv.ParseInt(r.FormValue("until"), 10, 32)
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	aliases := make(map[string][]string)
	targets := make([]string, 0)

	for t := 0; t < len(r.Form["target"]); t++ {
		target := r.Form["target"][t]
		if len(target) == 0 {
			continue
		}
		targets = append(targets, target)

		// Search in small index table first
		fndResult, err := finder.Find(h.config, r.Context(), target, fromTimestamp, untilTimestamp)
		if err != nil {
			clickhouse.HandleError(w, err)
			return
		}

		fndSeries := fndResult.Series()

		for i := 0; i < len(fndSeries); i++ {
			key := string(fndSeries[i])
			abs := string(fndResult.Abs(fndSeries[i]))
			if x, ok := aliases[key]; ok {
				aliases[key] = append(x, abs, target)
			} else {
				aliases[key] = []string{abs, target}
			}
		}
	}

	logger.Info("finder", zap.Int("metrics", len(aliases)))

	pointsTable, isReverse, rollupObj := SelectDataTable(h.config, fromTimestamp, untilTimestamp, targets, config.ContextGraphite)
	if pointsTable == "" {
		logger.Error("data tables is not specified", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var maxStep uint32

	now := time.Now().Unix()
	age := uint32(dry.Max(0, now-fromTimestamp))

	// make Path IN (...), calculate max step
	metricList := make([]string, 0, len(aliases))
	for m := range aliases {
		if len(m) == 0 {
			continue
		}

		step, _ := rollupObj.Lookup(m, age)
		if step > maxStep {
			maxStep = step
		}

		if isReverse {
			metricList = append(metricList, reverse.String(m))
		} else {
			metricList = append(metricList, m)
		}
	}

	if len(metricList) == 0 {
		// Return empty response
		h.Reply(w, r, EmptyData, 0, 0, "", nil)
		return
	}

	preWhere := where.New()
	preWhere.Andf(
		"Date >='%s' AND Date <= '%s'",
		time.Unix(fromTimestamp, 0).Format("2006-01-02"),
		time.Unix(untilTimestamp, 0).Format("2006-01-02"),
	)

	wr := where.New()
	wr.And(where.In("Path", metricList))

	until := untilTimestamp - untilTimestamp%int64(maxStep) + int64(maxStep) - 1
	wr.Andf("Time >= %d AND Time <= %d", fromTimestamp, until)

	query := fmt.Sprintf(`SELECT Path, Time, Value, Timestamp FROM %s %s %s FORMAT RowBinary`,
		pointsTable, preWhere.PreWhereSQL(), wr.SQL(),
	)

	// start carbonlink request
	carbonlinkResponseRead := h.queryCarbonlink(r.Context(), logger, metricList)

	body, err := clickhouse.Reader(
		scope.WithTable(r.Context(), pointsTable),
		h.config.ClickHouse.Url,
		query,
		clickhouse.Options{Timeout: h.config.ClickHouse.DataTimeout.Value(), ConnectTimeout: h.config.ClickHouse.ConnectTimeout.Value()},
	)

	if err != nil {
		clickhouse.HandleError(w, err)
		return
	}

	// fetch carbonlink response
	carbonlinkData := carbonlinkResponseRead()

	parseStart := time.Now()

	// pass carbonlinkData to DataParse
	data, err := DataParse(body, carbonlinkData, isReverse)

	if err != nil {
		logger.Error("data", zap.Error(err), zap.Int("read_bytes", data.length))
		clickhouse.HandleError(w, err)
		return
	}
	logger.Info("render", zap.Int("read_bytes", data.length), zap.Int("read_points", data.Points.Len()))

	d := time.Since(parseStart)
	logger.Debug("parse", zap.String("runtime", d.String()), zap.Duration("runtime_ns", d))

	sortStart := time.Now()
	data.Points.Sort()
	d = time.Since(sortStart)
	logger.Debug("sort", zap.String("runtime", d.String()), zap.Duration("runtime_ns", d))

	data.Points.Uniq()
	data.Aliases = aliases

	// pp.Println(points)
	h.Reply(w, r, data, uint32(fromTimestamp), uint32(untilTimestamp), prefix, rollupObj)
}

func (h *Handler) Reply(w http.ResponseWriter, r *http.Request, data *Data, from, until uint32, prefix string, rollupObj *rollup.Rules) {
	start := time.Now()
	switch r.FormValue("format") {
	case "pickle":
		h.ReplyPickle(w, r, data, from, until, prefix, rollupObj)
	case "protobuf":
		h.ReplyProtobuf(w, r, data, from, until, prefix, rollupObj)
	}
	d := time.Since(start)
	scope.Logger(r.Context()).Debug("reply", zap.String("runtime", d.String()), zap.Duration("runtime_ns", d))
}
