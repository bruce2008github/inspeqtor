package daemon

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mperham/inspeqtor/metrics"
	"github.com/mperham/inspeqtor/util"
)

func init() {
	metrics.Sources["postgresql"] = buildPostgresqlSource
}

type pgSource struct {
	Database    string
	Hostname    string
	Port        string
	Username    string
	metrics     map[string]bool
	captureRepl bool
	execFunk    executor
}

func (pg *pgSource) Name() string {
	return "postgresql"
}

func (pg *pgSource) Watch(metricName string) {
	pg.metrics[metricName] = true
}

func (pg *pgSource) Capture() (metrics.Map, error) {
	data := metrics.Map{}

	for name := range pg.metrics {
		if _, ok := data[name]; !ok {
			err := populate(pg, data, name)
			if err != nil {
				return nil, err
			}
		}
	}

	return data, nil
}

func (pg *pgSource) Prepare() error {
	_, err := runSql(pg, "select 1")
	return err
}

func (pg *pgSource) ValidMetrics() []metrics.Descriptor {
	return pgMetrics
}

// Password must be specified via a ~/.pgpass file
// http://www.postgresql.org/docs/current/static/libpq-pgpass.html
func (pg *pgSource) buildArgs() []string {
	args := []string{"-Xt"}

	if pg.Database != "" {
		args = append(args, "-d")
		args = append(args, pg.Database)
	}

	if pg.Hostname != "" {
		args = append(args, "-h")
		args = append(args, pg.Hostname)
	}

	if pg.Port != "" {
		args = append(args, "-p")
		args = append(args, pg.Port)
	}

	if pg.Username != "" {
		args = append(args, "-U")
		args = append(args, pg.Username)
	}

	return args
}

func buildPostgresqlSource(params map[string]string) (metrics.Source, error) {
	rs := &pgSource{"", "localhost", "5432", "postgres", map[string]bool{}, false, execCmd}
	for k, v := range params {
		switch k {
		case "database":
			rs.Database = v
		case "hostname":
			rs.Hostname = v
		case "username":
			rs.Username = v
		case "port":
			_, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return nil, err
			}
			rs.Port = v
		}
	}
	return rs, nil
}

func populate(pg *pgSource, data metrics.Map, name string) error {
	var sqlfunk func(*pgSource, metrics.Map) error
	switch name {
	case "rollbacks", "deadlocks", "numbackends", "blk_hit_rate":
		sqlfunk = dbStats
	case "seq_scans":
		sqlfunk = userStats
	case "total_size":
		sqlfunk = sizeStats
	default:
		return errors.New("No such metric: " + name)
	}
	return sqlfunk(pg, data)
}

func userStats(pg *pgSource, data metrics.Map) error {
	sql := "select sum(seq_scan) from pg_stat_user_tables"
	results, err := runSql(pg, sql)
	if err != nil {
		return err
	}
	if len(results) != 1 {
		return fmt.Errorf("Results size == %d", len(results))
	}
	if len(results[0]) != 1 {
		return fmt.Errorf("Results row size == %d", len(results[0]))
	}

	val := results[0][0]
	if val == "" {
		val = "0"
	}
	ival, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return err
	}
	data["seq_scans"] = float64(ival)
	return nil
}

func sizeStats(pg *pgSource, data metrics.Map) error {
	sql := `select sum(pg_total_relation_size(pg_class.oid))
					FROM pg_class LEFT JOIN pg_namespace N ON (N.oid = pg_class.relnamespace)
					WHERE nspname NOT IN ('pg_catalog', 'information_schema') AND
								nspname !~ '^pg_toast' AND relkind IN ('r')`
	results, err := runSql(pg, sql)
	if err != nil {
		return err
	}
	if len(results) != 1 {
		return fmt.Errorf("Results size == %d", len(results))
	}
	if len(results[0]) != 1 {
		return fmt.Errorf("Results row size == %d", len(results[0]))
	}

	val := results[0][0]
	if val == "" {
		val = "0"
	}
	ival, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return err
	}
	data["total_size"] = float64(ival)
	return nil
}

func dbStats(pg *pgSource, data metrics.Map) error {
	sql := "select sum(xact_rollback), sum(deadlocks), sum(numbackends), sum(blks_hit) / (sum(blks_read) + sum(blks_hit)) from pg_stat_database"
	results, err := runSql(pg, sql)
	if err != nil {
		return err
	}
	if len(results) != 1 {
		return fmt.Errorf("Results size == %d", len(results))
	}
	if len(results[0]) != 4 {
		return fmt.Errorf("Results row size == %d", len(results[0]))
	}

	if _, ok := pg.metrics["rollbacks"]; ok {
		val := results[0][0]
		ival, err := strconv.ParseUint(val, 10, 64)
		if err != nil {
			return err
		}
		data["rollbacks"] = float64(ival)
	}

	if _, ok := pg.metrics["deadlocks"]; ok {
		val := results[0][1]
		ival, err := strconv.ParseUint(val, 10, 64)
		if err != nil {
			return err
		}
		data["deadlocks"] = float64(ival)
	}

	if _, ok := pg.metrics["numbackends"]; ok {
		val := results[0][2]
		fval, err := strconv.ParseUint(val, 10, 64)
		if err != nil {
			return err
		}
		data["numbackends"] = float64(fval)
	}

	if _, ok := pg.metrics["blk_hit_rate"]; ok {
		val := results[0][3]
		fval, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return err
		}
		data["blk_hit_rate"] = fval * 100
	}

	return nil
}

func runSql(pg *pgSource, stmt string) ([][]string, error) {
	args := pg.buildArgs()
	args = append(args, "-c")
	args = append(args, stmt)
	sout, err := pg.execFunk("psql", args, nil)
	if err != nil {
		return nil, errors.New(string(sout))
	}

	lines, err := util.ReadLines(sout)
	if err != nil {
		return nil, err
	}

	var table [][]string

	for _, line := range lines {
		if line == "" {
			continue
		}
		var row []string
		cells := strings.Split(line, "|")
		for _, cell := range cells {
			row = append(row, strings.TrimSpace(cell))
		}
		table = append(table, row)
	}

	return table, nil
}

var (
	pgMetrics = []metrics.Descriptor{
		metrics.Descriptor{"rollbacks", c, nil, nil},
		metrics.Descriptor{"deadlocks", c, nil, nil},
		metrics.Descriptor{"numbackends", g, nil, nil},
		metrics.Descriptor{"blk_hit_rate", g, metrics.DisplayPercent, nil},
		metrics.Descriptor{"seq_scans", c, nil, nil},
		metrics.Descriptor{"total_size", g, inMB, nil},
	}
)
