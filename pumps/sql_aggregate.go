package pumps

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/TykTechnologies/tyk-pump/analytics"

	"github.com/mitchellh/mapstructure"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gorm_logger "gorm.io/gorm/logger"
)

type SQLAggregatePumpConf struct {
	SQLConf `mapstructure:",squash"`

	EnvPrefix               string   `mapstructure:"meta_env_prefix"`
	TrackAllPaths           bool     `mapstructure:"track_all_paths"`
	IgnoreTagPrefixList     []string `mapstructure:"ignore_tag_prefix_list"`
	ThresholdLenTagList     int      `mapstructure:"threshold_len_tag_list"`
	StoreAnalyticsPerMinute bool     `mapstructure:"store_analytics_per_minute"`
	IgnoreAggregationsList  []string `mapstructure:"ignore_aggregations"`
}

type SQLAggregatePump struct {
	CommonPumpConfig

	SQLConf *SQLAggregatePumpConf

	db      *gorm.DB
	dbType  string
	dialect gorm.Dialector
}

var SQLAggregatePumpPrefix = "SQL-aggregate-pump"
var SQLAggregateDefaultENV = PUMPS_ENV_PREFIX + "_SQLAGGREGATE" + PUMPS_ENV_META_PREFIX

func (c *SQLAggregatePump) New() Pump {
	newPump := SQLAggregatePump{}
	return &newPump
}

func (c *SQLAggregatePump) GetName() string {
	return "SQL Aggregate Pump"
}

func (c *SQLAggregatePump) GetEnvPrefix() string {
	return c.SQLConf.EnvPrefix
}

func (c *SQLAggregatePump) Init(conf interface{}) error {
	c.SQLConf = &SQLAggregatePumpConf{}
	c.log = log.WithField("prefix", SQLAggregatePumpPrefix)

	err := mapstructure.Decode(conf, &c.SQLConf)
	if err != nil {
		c.log.Error("Failed to decode configuration: ", err)
		return err
	}

	processPumpEnvVars(c, c.log, c.SQLConf, SQLAggregateDefaultENV)

	logLevel := gorm_logger.Silent

	switch c.SQLConf.LogLevel {
	case "debug":
		logLevel = gorm_logger.Info
	case "info":
		logLevel = gorm_logger.Warn
	case "warning":
		logLevel = gorm_logger.Error
	}

	dialect, errDialect := Dialect(&c.SQLConf.SQLConf)
	if errDialect != nil {
		c.log.Error(errDialect)
		return errDialect
	}
	db, err := gorm.Open(dialect, &gorm.Config{
		AutoEmbedd:  true,
		UseJSONTags: true,
		Logger:      gorm_logger.Default.LogMode(logLevel),
	})

	if err != nil {
		c.log.Error(err)
		return err
	}
	c.db = db
	if !c.SQLConf.TableSharding {
		c.db.Table(analytics.AggregateSQLTable).AutoMigrate(&analytics.SQLAnalyticsRecordAggregate{})
	}

	if c.SQLConf.BatchSize == 0 {
		c.SQLConf.BatchSize = SQLDefaultQueryBatchSize
	}

	c.log.Debug("SQLAggregate Initialized")
	return nil
}

// WriteData aggregates and writes the passed data to SQL database. When table sharding is enabled, startIndex and endIndex
// are found by checking timestamp of the records. The main for loop iterates and finds the index where a new day starts.
// Then, the data is passed to AggregateData function and written to database day by day on different tables. However,
// if table sharding is not enabled, the for loop iterates one time and all data is passed at once to the AggregateData
// function and written to database on single table.
func (c *SQLAggregatePump) WriteData(ctx context.Context, data []interface{}) error {
	dataLen := len(data)
	c.log.Debug("Attempting to write ", dataLen, " records...")

	if dataLen == 0 {
		return nil
	}

	startIndex := 0
	endIndex := dataLen
	table := ""
	for i := 0; i <= dataLen; i++ {
		if c.SQLConf.TableSharding {
			recDate := data[startIndex].(analytics.AnalyticsRecord).TimeStamp.Format("20060102")
			var nextRecDate string
			//if we're on i == dataLen iteration, it means that we're out of index range. We're going to use the last record date.
			if i == dataLen {
				nextRecDate = data[dataLen-1].(analytics.AnalyticsRecord).TimeStamp.Format("20060102")
			} else {
				nextRecDate = data[i].(analytics.AnalyticsRecord).TimeStamp.Format("20060102")

				//if both dates are equal, we shouldn't write in the table yet.
				if recDate == nextRecDate {
					continue
				}
			}

			endIndex = i

			table = analytics.AggregateSQLTable + "_" + recDate
			c.db = c.db.Table(table)
			if !c.db.Migrator().HasTable(table) {
				c.db.AutoMigrate(&analytics.SQLAnalyticsRecordAggregate{})
			}
		} else {
			i = dataLen // write all records at once for non-sharded case, stop for loop after 1 iteration
			table = analytics.AggregateSQLTable
		}

		analyticsPerOrg := analytics.AggregateData(data[startIndex:endIndex], c.SQLConf.TrackAllPaths, c.SQLConf.IgnoreTagPrefixList, c.SQLConf.StoreAnalyticsPerMinute)

		for orgID, ag := range analyticsPerOrg {
			recs := []analytics.SQLAnalyticsRecordAggregate{}
			for _, d := range ag.Dimensions() {
				id := fmt.Sprintf("%v", ag.TimeStamp.Unix()) + orgID + d.Name + d.Value
				uID := hex.EncodeToString([]byte(id))

				rec := analytics.SQLAnalyticsRecordAggregate{
					ID:             uID,
					OrgID:          orgID,
					TimeStamp:      ag.TimeStamp.Unix(),
					Counter:        *d.Counter,
					Dimension:      d.Name,
					DimensionValue: d.Value,
				}
				rec.ProcessStatusCodes(rec.Counter.ErrorMap)
				rec.Counter.ErrorList = nil
				rec.Counter.ErrorMap = nil
				recs = append(recs, rec)
			}

			for i := 0; i < len(recs); i += c.SQLConf.BatchSize {
				ends := i + c.SQLConf.BatchSize
				if ends > len(recs) {
					ends = len(recs)
				}

				//we use excluded as temp  table since it's supported by our SQL storages https://www.postgresql.org/docs/9.5/sql-insert.html#SQL-ON-CONFLICT  https://www.sqlite.org/lang_UPSERT.html
				tx := c.db.WithContext(ctx).Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "id"}},
					DoUpdates: clause.Assignments(analytics.OnConflictAssignments(table, "excluded")),
				}).Create(recs[i:ends])
				if tx.Error != nil {
					c.log.Error("error writing aggregated records into "+table+":", tx.Error)
					return tx.Error
				}
			}
		}

		startIndex = i // next day start index, necessary for sharded case
	}

	c.log.Info("Purged ", dataLen, " records...")

	return nil
}
