package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	db "github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"strings"
	"time"
)

// postgresTableItem stores the value of a resource and its type.
// Allows storage of any type and simpler table creation
type postgresTableItem struct {
	Value    interface{} `json:"value"`
	ItemType string      `json:"type"`
}

type postgresOfflineStore struct {
	conn *pgxpool.Pool
	ctx  context.Context
	BaseProvider
}

type PostgresConfig struct {
	Host     string
	Port     string
	Username string
	Password string
	Database string
}

func sanitize(ident string) string {
	return db.Identifier{ident}.Sanitize()
}

func (pg *PostgresConfig) Deserialize(config SerializedConfig) error {
	err := json.Unmarshal(config, pg)
	if err != nil {
		return err
	}
	return nil
}

func (pg *PostgresConfig) Serialize() []byte {
	conf, err := json.Marshal(pg)
	if err != nil {
		panic(err)
	}
	return conf
}

func postgresOfflineStoreFactory(config SerializedConfig) (Provider, error) {
	pg := PostgresConfig{}
	if err := pg.Deserialize(config); err != nil {
		return nil, errors.New("invalid postgres config")
	}

	store, err := NewPostgresOfflineStore(pg)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// NewPostgresOfflineStore creates a connection to a postgres database
// and initializes a table to track currently active Resource tables.
func NewPostgresOfflineStore(pg PostgresConfig) (*postgresOfflineStore, error) {
	url := fmt.Sprintf("postgres://%s:%s@%s:%s/%s", pg.Username, pg.Password, pg.Host, pg.Port, pg.Database)
	ctx := context.Background()
	conn, err := pgxpool.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	return &postgresOfflineStore{
		conn: conn,
		ctx:  ctx,
	}, nil
}

func (store *postgresOfflineStore) getResourceTableName(id ResourceID) string {
	var idType string
	if id.Type == Feature {
		idType = "feature"
	} else {
		idType = "label"
	}
	return fmt.Sprintf("featureform_resource_%s_%s_%s", idType, id.Name, id.Variant)
}

func (store *postgresOfflineStore) getMaterializationTableName(ftID MaterializationID) string {
	return fmt.Sprintf("featureform_materialization_%s", ftID)
}

func (store *postgresOfflineStore) getTrainingSetName(id ResourceID) string {
	return fmt.Sprintf("featureform_trainingset_%s_%s", id.Name, id.Variant)
}

func (store *postgresOfflineStore) tableExists(id ResourceID) (bool, error) {
	var n int64
	var tableName string
	if id.check(Feature, Label) == nil {
		tableName = store.getResourceTableName(id)
	} else if id.check(TrainingSet) == nil {
		tableName = store.getTrainingSetName(id)
	}
	err := store.conn.QueryRow(context.Background(), "SELECT 1 FROM information_schema.tables WHERE table_name=$1", tableName).Scan(&n)
	if err == db.ErrNoRows {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func (store *postgresOfflineStore) AsOfflineStore() (OfflineStore, error) {
	return store, nil
}

// CreateResourceTable creates a new Resource table.
// Returns a table if it does not already exist and stores the table ID in the resource index table.
// Returns an error if the table already exists or if table is the wrong type.
func (store *postgresOfflineStore) CreateResourceTable(id ResourceID) (OfflineTable, error) {
	if err := id.check(Feature, Label); err != nil {
		return nil, err
	}

	if exists, err := store.tableExists(id); err != nil {
		return nil, err
	} else if exists {
		return nil, &TableAlreadyExists{id.Name, id.Variant}
	}
	tableName := store.getResourceTableName(id)
	table, err := newPostgresOfflineTable(store.conn, tableName)
	if err != nil {
		return nil, err
	}
	return table, nil
}

func (store *postgresOfflineStore) GetResourceTable(id ResourceID) (OfflineTable, error) {
	return store.getPostgresResourceTable(id)
}

func (store *postgresOfflineStore) getPostgresResourceTable(id ResourceID) (*postgresOfflineTable, error) {
	if exists, err := store.tableExists(id); err != nil {
		return nil, err
	} else if !exists {
		return nil, &TableNotFound{id.Name, id.Variant}
	}
	return &postgresOfflineTable{
		conn: store.conn,
		ctx:  store.ctx,
		name: store.getResourceTableName(id),
	}, nil
}

func (store *postgresOfflineStore) CreateMaterialization(id ResourceID) (Materialization, error) {
	if id.Type != Feature {
		return nil, errors.New("only features can be materialized")
	}
	resTable, err := store.getPostgresResourceTable(id)
	if err != nil {
		return nil, err
	}

	matID := MaterializationID(id.Name)
	matTableName := store.getMaterializationTableName(matID)
	sanitizedTableName := sanitize(matTableName)
	resTableName := sanitize(resTable.name)
	tableCreateQry := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s AS (SELECT entity, value, ts FROM %s WHERE 1=2)", sanitizedTableName, resTableName)

	_, err = store.conn.Exec(
		context.Background(), tableCreateQry)
	if err != nil {
		return nil, err
	}

	materializeQry := fmt.Sprintf(
		"INSERT INTO %s SELECT entity, value, ts FROM "+
			"(SELECT entity, ts, value, row_number() OVER (PARTITION BY entity ORDER BY ts desc) "+
			"AS rn FROM %s) t WHERE rn=1", sanitizedTableName, resTableName)

	_, err = store.conn.Exec(context.Background(), materializeQry)
	if err != nil {
		return nil, err
	}

	return &postgresMaterialization{
		id:        matID,
		conn:      store.conn,
		tableName: matTableName,
	}, nil

}
func (store *postgresOfflineStore) GetMaterialization(id MaterializationID) (Materialization, error) {
	tableName := store.getMaterializationTableName(id)
	getMatQry := fmt.Sprintf("SELECT DISTINCT (table_name) FROM information_schema.tables WHERE table_name=$1")
	rows, err := store.conn.Query(context.Background(), getMatQry, tableName)
	defer rows.Close()
	if err != nil {
		return nil, err
	}
	rowCount := 0
	if rows.Next() {
		rowCount++
	}
	if rowCount == 0 {
		return nil, &MaterializationNotFound{id}
	}
	return &postgresMaterialization{
		id:        id,
		conn:      store.conn,
		ctx:       store.ctx,
		tableName: tableName,
	}, err
}

func (store *postgresOfflineStore) CreateTrainingSet(def TrainingSetDef) error {
	if err := def.check(); err != nil {
		return err
	}
	label, err := store.getPostgresResourceTable(def.Label)
	if err != nil {
		return err
	}
	tableName := store.getTrainingSetName(def.ID)

	columns := make([]string, 0)
	query := fmt.Sprintf(" (SELECT entity, value , ts from %s ) l ", sanitize(label.name))
	for i, feature := range def.Features {
		resourceTableName := sanitize(store.getResourceTableName(feature))
		tableJoinAlias := fmt.Sprintf("t%d", i)
		columns = append(columns, resourceTableName)
		query = fmt.Sprintf("%s LEFT JOIN LATERAL (SELECT entity , value as %s, ts  FROM %s WHERE entity=l.entity and ts <= l.ts ORDER BY ts desc LIMIT 1) %s on %s.entity=l.entity ",
			query, resourceTableName, resourceTableName, tableJoinAlias, tableJoinAlias)
		if i == len(def.Features)-1 {
			query = fmt.Sprintf("%s )", query)
		}
	}
	columnStr := strings.Join(columns, ", ")
	fullQuery := fmt.Sprintf("CREATE TABLE %s AS (SELECT %s, l.value as label FROM %s ", sanitize(tableName), columnStr, query)

	if _, err := store.conn.Exec(context.Background(), fullQuery); err != nil {
		return err
	}
	return nil
}

func (store *postgresOfflineStore) GetTrainingSet(id ResourceID) (TrainingSetIterator, error) {
	if err := id.check(TrainingSet); err != nil {
		return nil, err
	}
	if exists, err := store.tableExists(id); err != nil {
		return nil, err
	} else if !exists {
		return nil, &TrainingSetNotFound{id}
	}
	trainingSetName := store.getTrainingSetName(id)
	rows, err := store.conn.Query(
		context.Background(),
		"SELECT column_name FROM information_schema.columns WHERE table_name = $1 order by ordinal_position",
		trainingSetName)
	defer rows.Close()
	if err != nil {
		return nil, err
	}
	features := make([]string, 0)
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, err
		}
		features = append(features, sanitize(column))
	}
	columns := strings.Join(features[:], ", ")

	trainingSetQry := fmt.Sprintf("SELECT %s FROM %s", columns, sanitize(trainingSetName))
	rows, err = store.conn.Query(context.Background(), trainingSetQry)
	if err != nil {
		return nil, err
	}

	return newPostgresTrainingSetIterator(rows), nil
}

type postgresTrainingRowsIterator struct {
	rows            db.Rows
	currentFeatures []interface{}
	currentLabel    interface{}
	err             error
}

func newPostgresTrainingSetIterator(rows db.Rows) TrainingSetIterator {
	return &postgresTrainingRowsIterator{
		rows:            rows,
		currentFeatures: nil,
		currentLabel:    nil,
		err:             nil,
	}
}

func (it *postgresTrainingRowsIterator) Next() bool {
	if !it.rows.Next() {
		it.rows.Close()
		return false
	}
	var label interface{}
	values, err := it.rows.Values()
	if err != nil {
		it.rows.Close()
		it.err = err
		return false
	}
	numFeatures := len(values) - 1
	featureVals := make([]interface{}, numFeatures)
	for i, value := range values {
		if i < numFeatures {
			if value == nil {
				featureVals[i] = value
			} else {
				featureVals[i] = it.parseTableValue(value)
			}
		} else {
			if value == nil {
				label = value
			} else {
				label = it.parseTableValue(value)
			}
		}
	}
	it.currentFeatures = featureVals
	it.currentLabel = label
	return true
}

func (it *postgresTrainingRowsIterator) Err() error {
	return it.err
}

func (it *postgresTrainingRowsIterator) Features() []interface{} {
	return it.currentFeatures
}

func (it *postgresTrainingRowsIterator) Label() interface{} {
	return it.currentLabel
}

func (it *postgresTrainingRowsIterator) parseTableValue(value interface{}) interface{} {
	v := value.(map[string]interface{})
	item := postgresTableItem{
		Value:    v["value"],
		ItemType: v["type"].(string),
	}
	return castTableItemType(item)
}

func (store *postgresOfflineStore) deserialize(v []byte) (postgresTableItem, error) {
	item := postgresTableItem{}
	if err := json.Unmarshal(v, &item); err != nil {
		return postgresTableItem{}, err
	}
	return item, nil
}

type postgresOfflineTable struct {
	conn *pgxpool.Pool
	ctx  context.Context
	name string
}

func newPostgresOfflineTable(conn *pgxpool.Pool, name string) (*postgresOfflineTable, error) {
	tableCreateQry := fmt.Sprintf("CREATE TABLE %s (entity VARCHAR, value JSONB, ts timestamptz, UNIQUE (entity, ts))", sanitize(name))
	_, err := conn.Exec(context.Background(), tableCreateQry)
	if err != nil {
		return nil, err
	}
	return &postgresOfflineTable{
		conn: conn,
		name: name,
	}, nil
}

func (table *postgresOfflineTable) Write(rec ResourceRecord) error {
	tb := sanitize(table.name)
	if err := rec.check(); err != nil {
		return err
	}
	value, err := table.serialize(rec.Value)
	if err != nil {
		return err
	}
	upsertQuery := fmt.Sprintf(""+
		"INSERT INTO %s (entity, value, ts) "+
		"VALUES ($1, $2, $3) "+
		"ON CONFLICT (entity, ts)"+
		"DO UPDATE SET value=$2 WHERE excluded.entity=$1 AND excluded.ts=$3", tb)
	if _, err := table.conn.Exec(context.Background(), upsertQuery, rec.Entity, value, rec.TS); err != nil {
		return err
	}

	return nil
}

func (table *postgresOfflineTable) resourceExists(rec ResourceRecord) (bool, error) {
	query := fmt.Sprintf("SELECT entity, value, ts FROM %s WHERE entity=$1 AND ts=$2 ", sanitize(table.name))
	rows, err := table.conn.Query(context.Background(), query, rec.Entity, rec.TS)
	defer rows.Close()
	if err != nil {
		return false, err
	}
	rowCount := 0
	for rows.Next() {
		rowCount++
	}
	if rowCount == 0 {
		return false, nil
	}
	return true, nil
}

func (table *postgresOfflineTable) serialize(v interface{}) ([]byte, error) {
	item := postgresTableItem{
		Value:    v,
		ItemType: fmt.Sprintf("%T", v),
	}
	return json.Marshal(item)
}

func (table *postgresOfflineTable) deserialize(v []byte) (interface{}, error) {
	item := postgresTableItem{}
	if err := json.Unmarshal(v, &item); err != nil {
		return nil, err
	}
	return item.Value, nil
}

type postgresMaterialization struct {
	id        MaterializationID
	conn      *pgxpool.Pool
	ctx       context.Context
	tableName string
	data      []ResourceRecord
}

func (mat *postgresMaterialization) ID() MaterializationID {
	return mat.id
}

func (mat *postgresMaterialization) NumRows() (int64, error) {
	n := int64(0)
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s", sanitize(mat.tableName))
	rows := mat.conn.QueryRow(context.Background(), query)
	err := rows.Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (mat *postgresMaterialization) IterateSegment(start, end int64) (FeatureIterator, error) {
	query := fmt.Sprintf(""+
		"SELECT entity, value, ts::timestamptz FROM "+
		"( SELECT * FROM "+
		"( SELECT *, row_number() over() FROM %s )t1 WHERE row_number>$1 AND row_number<=$2)t2", sanitize(mat.tableName))
	rows, err := mat.conn.Query(context.Background(), query, start, end)
	if err != nil {
		return nil, err
	}

	return newPostgresFeatureIterator(rows), nil
}

type postgresFeatureIterator struct {
	rows         db.Rows
	err          error
	currentValue ResourceRecord
}

func newPostgresFeatureIterator(rows db.Rows) FeatureIterator {
	return &postgresFeatureIterator{
		rows:         rows,
		err:          nil,
		currentValue: ResourceRecord{},
	}
}

func (iter *postgresFeatureIterator) Next() bool {
	if !iter.rows.Next() {
		iter.rows.Close()
		return false
	}
	var rec ResourceRecord
	var value []byte
	var ts time.Time
	if err := iter.rows.Scan(&rec.Entity, &value, &ts); err != nil {
		iter.rows.Close()
		iter.err = err
		return false
	}
	val, err := iter.deserialize(value)
	if err != nil {
		iter.rows.Close()
		iter.err = err
		return false
	}
	rec.Value = castTableItemType(val)
	rec.TS = ts.UTC()
	iter.currentValue = rec
	return true
}

func (iter *postgresFeatureIterator) Value() ResourceRecord {
	return iter.currentValue
}

func (iter *postgresFeatureIterator) Err() error {
	return nil
}

func (iter *postgresFeatureIterator) deserialize(v []byte) (postgresTableItem, error) {
	item := postgresTableItem{}
	if err := json.Unmarshal(v, &item); err != nil {
		return postgresTableItem{}, err
	}
	return item, nil
}

// castTableItemType returns the value casted as its original type
func castTableItemType(v postgresTableItem) interface{} {
	switch v.ItemType {
	case "int":
		return int(v.Value.(float64))
	case "int8":
		return int8(v.Value.(float64))
	case "int16":
		return int16(v.Value.(float64))
	case "int32":
		return int32(v.Value.(float64))
	case "int64":
		return int64(v.Value.(float64))
	case "float32":
		return float32(v.Value.(float64))
	case "float64":
		return v.Value
	case "string":
		return v.Value.(string)
	case "bool":
		return v.Value.(bool)
	default:
		return v.Value
	}
}
