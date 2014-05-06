package store

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq"
	"github.com/lib/pq/hstore"
	"github.com/rcrowley/go-metrics"
)

type postgresStore struct {
	db *sql.DB
}

var schedulingInterval = flag.Duration("scheduling_interval", time.Minute, "run the scheduler this often.")

var noPriorFunctionsScheduledCounter = metrics.GetOrRegisterCounter("NoPriorFunctionsScheduled", nil)
var lastMaxPriorityErrorCounter = metrics.GetOrRegisterCounter("LastMaxPriorityError", nil)
var deleteExpiredFunctionsErrorCounter = metrics.GetOrRegisterCounter("DeleteExpiredFunctionsError", nil)
var countSchedluedTasksErrorCounter = metrics.GetOrRegisterCounter("CountSchedluedTasksError", nil)
var countScheduledFunctionsErrorCounter = metrics.GetOrRegisterCounter("CountScheduledFunctionsError", nil)
var unfilledScheduleCounter = metrics.GetOrRegisterCounter("UnfilledSchedule", nil)
var insertScheduledFunctionsErrorCounter = metrics.GetOrRegisterCounter("InsertScheduledFunctionsError", nil)
var emptyTaskFunctionCounter = metrics.GetOrRegisterCounter("EmptyTaskFunction", nil)

func openPostgres(db *sql.DB) Store {
	return &postgresStore{
		db: db,
	}
}

func (store *postgresStore) Close() {
	store.db.Close()
}

func insertTaskFunctions(tx *sql.Tx) error {
	var minTaskFunction, minPriority int
	row := tx.QueryRow("SELECT task_function, priority FROM scheduled_functions ORDER BY scheduled_time DESC, priority DESC, task_function DESC LIMIT 1")
	if err := row.Scan(&minTaskFunction, &minPriority); err == sql.ErrNoRows {
		log.Printf("no prior functions scheduled")
		noPriorFunctionsScheduledCounter.Inc(1)
	} else if err != nil {
		log.Printf("error finding max last priority: %v", err)
		lastMaxPriorityErrorCounter.Inc(1)
		return err
	}

	log.Printf("min task function: %v, min priority: %v", minTaskFunction, minPriority)

	if _, err := tx.Exec("DELETE FROM scheduled_functions WHERE expiration_time < now() OR measurements_remaining <= 0"); err != nil {
		log.Printf("error deleting expired task functions: %v", err)
		deleteExpiredFunctionsErrorCounter.Inc(1)
		return err
	}

	var toSchedule int
	row = tx.QueryRow("SELECT concurrent_functions - scheduled FROM (SELECT count(1) scheduled FROM scheduled_functions) AS c, scheduler_configuration")
	if err := row.Scan(&toSchedule); err != nil {
		log.Printf("error counting scheduled tasks: %v", err)
		countSchedluedTasksErrorCounter.Inc(1)
		return err
	}

	result, err := tx.Exec("INSERT INTO scheduled_functions (task_function, expiration_time, measurements_remaining, priority, scheduled_time) SELECT id, now() + max_duration_seconds * interval '1 second', max_measurements, priority, now() FROM task_functions WHERE enabled AND ((priority = $1 AND id > $2) OR priority > $1) ORDER BY priority, id LIMIT $3", minPriority, minTaskFunction, toSchedule)
	if err != nil {
		log.Printf("error inserting new schedules: %v", err)
		insertScheduledFunctionsErrorCounter.Inc(1)
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("error discovering number of affected rows: %v", err)
		countScheduledFunctionsErrorCounter.Inc(1)
		return err
	}
	toSchedule -= int(rowsAffected)
	result, err = tx.Exec("INSERT INTO scheduled_functions (task_function, expiration_time, measurements_remaining, priority, scheduled_time) SELECT id, now() + max_duration_seconds * interval '1 second', max_measurements, priority, now() FROM task_functions WHERE enabled ORDER BY priority, id LIMIT $1", toSchedule)
	if err != nil {
		log.Printf("error inserting new schedules: %v", err)
		insertScheduledFunctionsErrorCounter.Inc(1)
		return err
	}
	rowsAffected, err = result.RowsAffected()
	if err != nil {
		log.Printf("error discovering number of affected rows: %v", err)
		countScheduledFunctionsErrorCounter.Inc(1)
		return err
	}
	toSchedule -= int(rowsAffected)
	if toSchedule > 0 {
		log.Printf("unable to fill schedule")
		unfilledScheduleCounter.Inc(1)
	}
	return nil
}

func (store *postgresStore) ScheduleTaskFunctions() {
	schedule := func() {
		tx, err := store.db.Begin()
		if err != nil {
			log.Printf("error starting transaction: %v", err)
			return
		}
		if err := insertTaskFunctions(tx); err != nil {
			tx.Rollback()
			return
		}
		if err := tx.Commit(); err != nil {
			log.Printf("error committing transaction: %v", err)
			return
		}
	}

	schedule()
	for _ = range time.Tick(*schedulingInterval) {
		schedule()
	}
}

func (store *postgresStore) Tasks(taskRequests <-chan *TaskRequest) {
	updateTicker := time.Tick(*schedulingInterval)

	selectStmt, err := store.db.Prepare("SELECT task_functions.task_function FROM scheduled_functions JOIN task_functions ON scheduled_functions.task_function = id ORDER BY id")
	if err != nil {
		log.Fatalf("error preparing schedules select statement: %v", err)
	}

	refreshTaskFunctions := func() []string {
		rows, err := selectStmt.Query()
		if err != nil {
			log.Fatalf("error selecting schedules: %v", err)
		}
		var taskFunctions []string
		for rows.Next() {
			var taskFunction string
			if err := rows.Scan(&taskFunction); err != nil {
				log.Fatalf("error scanning task function: %v", err)
			}
			taskFunctions = append(taskFunctions, taskFunction)
		}
		return taskFunctions
	}

	currentTaskFunctions := refreshTaskFunctions()
	functionIdx := 0
	var currentTaskFunction string
	for {
		if len(currentTaskFunctions) > 0 {
			functionIdx = (functionIdx + 1) % len(currentTaskFunctions)
			currentTaskFunction = currentTaskFunctions[functionIdx]
		}

		select {
		case taskRequest := <-taskRequests:
			queryString := fmt.Sprintf("SELECT id, parameters FROM task_functions.%[1]s($1) ORDER BY random() LIMIT 1", currentTaskFunction)
			log.Println(queryString)
			hints := hstore.Hstore{
				Map: make(map[string]sql.NullString),
			}
			for k, v := range taskRequest.Hints {
				hints.Map[k] = sql.NullString{
					String: v,
					Valid:  true,
				}
			}
			row := store.db.QueryRow(queryString, hints)
			var task *Task
			var id int
			var parameters hstore.Hstore
			if err := row.Scan(&id, &parameters); err != nil {
				log.Printf("error scanning task parameters: %v", err)
			} else {
				task = new(Task)
				task.Id = id
				task.Parameters = parameters.Map
			}
			select {
			case taskRequest.Response <- task:
			case <-time.After(time.Second):
				log.Printf("task response timed out")
			}
			close(taskRequest.Response)

		case <-updateTicker:
			currentTaskFunctions = refreshTaskFunctions()
			functionIdx = 0
		}
	}
}

func (store *postgresStore) WriteTasks(tasks <-chan *Task) {
	tasksStmt, err := store.db.Prepare("INSERT INTO tasks (parameters) VALUES ($1)")
	if err != nil {
		log.Fatalf("error preparing tasks insert statement: %v", err)
	}
	defer tasksStmt.Close()

	for task := range tasks {
		parameters := hstore.Hstore{task.Parameters}
		tasksStmt.Exec(parameters)
	}

	if err := tasksStmt.Close(); err != nil {
		log.Printf("error while closing tasks insert statement: %v", err)
	}
}

func (store *postgresStore) WriteQueries(queries <-chan *Query) {
	queriesStmt, err := store.db.Prepare("INSERT INTO queries (timestamp, client_ip, task, raw_request, substrate, parameters_json, response_body) VALUES ($1, $2, $3, $4, $5, $6, $7)")
	if err != nil {
		log.Fatalf("error preparing queries insert statement: %v", err)
	}
	defer queriesStmt.Close()

	for query := range queries {
		if _, err := queriesStmt.Exec(query.Timestamp, query.RemoteAddr, query.Task, query.RawRequest, query.Substrate, query.ParametersJson, query.ResponseBody); err != nil {
			log.Printf("error inserting query: %v", err)
			continue
		}
	}

	if err := queriesStmt.Close(); err != nil {
		log.Printf("error while closing queries insert statement: %v", err)
	}
}

func (store *postgresStore) Queries() <-chan *Query {
	queries := make(chan *Query)
	go func() {
		defer close(queries)

		rows, err := store.db.Query("SELECT id, timestamp, client_ip, task, raw_request, substrate, parameters_json FROM queries")
		if err != nil {
			log.Fatalf("error selecting queries: %v", err)
		}
		defer rows.Close()

		for rows.Next() {
			var query Query
			if err := rows.Scan(&query.Id, &query.Timestamp, &query.RemoteAddr, &query.Task, &query.RawRequest, &query.Substrate, &query.ParametersJson); err != nil {
				log.Printf("error reading query: %v", err)
			}
			queries <- &query
		}
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows after reading queries: %v", err)
		}
	}()
	return queries
}

func (store *postgresStore) UnparsedQueries() <-chan *Query {
	queries := make(chan *Query)
	go func() {
		defer close(queries)

		rows, err := store.db.Query("SELECT id, timestamp, client_ip, task, raw_request, substrate, parameters_json FROM queries WHERE NOT EXISTS (SELECT NULL FROM parsed_queries WHERE query = id)")
		if err != nil {
			log.Fatalf("error selecting queries: %v", err)
		}
		defer rows.Close()

		for rows.Next() {
			var query Query
			if err := rows.Scan(&query.Id, &query.Timestamp, &query.RemoteAddr, &query.Task, &query.RawRequest, &query.Substrate, &query.ParametersJson); err != nil {
				log.Printf("error reading query: %v", err)
			}
			queries <- &query
		}
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows after reading unparsed queries: %v", err)
		}
	}()
	return queries
}

func (store *postgresStore) WriteParsedQueries(parsedQueries <-chan *ParsedQuery) {
	insertIntoQueries, err := store.db.Prepare(`INSERT INTO parsed_queries (query, measurement_id, timestamp, client_ip, client_location, substrate, parameters) VALUES ($1, $2, $3, $4, $5, $6, $7)`)
	if err != nil {
		log.Fatalf("error preparing parsed_queries insert statement: %v", err)
	}
	defer insertIntoQueries.Close()

	for parsedQuery := range parsedQueries {
		if _, err := insertIntoQueries.Exec(parsedQuery.Query, parsedQuery.MeasurementId, parsedQuery.Timestamp, parsedQuery.ClientIp.String(), parsedQuery.ClientLocation, parsedQuery.Substrate, hstore.Hstore{parsedQuery.Parameters}); err != nil {
			log.Printf("error inserting parsed query: %v", err)
		}
	}

	if err := insertIntoQueries.Close(); err != nil {
		log.Printf("error while closing parsed queries insert statement: %v", err)
	}
}

func (store *postgresStore) WriteResults(results <-chan *Result) {
	resultsStmt, err := store.db.Prepare("INSERT INTO results (timestamp, client_ip, raw_request) VALUES ($1, $2, $3)")
	if err != nil {
		log.Fatalf("error preparing results insert statement: %v", err)
	}
	defer resultsStmt.Close()

	for result := range results {
		if _, err := resultsStmt.Exec(result.Timestamp, result.RemoteAddr, result.RawRequest); err != nil {
			log.Printf("error inserting result: %v", err)
			continue
		}
	}

	if err := resultsStmt.Close(); err != nil {
		log.Printf("error while closing results insert statement: %v", err)
	}
}

func (store *postgresStore) Results() <-chan *Result {
	results := make(chan *Result)
	go func() {
		defer close(results)

		rows, err := store.db.Query("SELECT id, timestamp, client_ip, raw_request FROM results")
		if err != nil {
			log.Fatalf("error selecting results: %v", err)
		}
		for rows.Next() {
			var result Result
			if err := rows.Scan(&result.Id, &result.Timestamp, &result.RemoteAddr, &result.RawRequest); err != nil {
				log.Printf("error scanning result: %v", err)
			}
			results <- &result
		}
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows after selecting results: %v", err)
		}
	}()
	return results
}

func (store *postgresStore) UnparsedResults() <-chan *Result {
	results := make(chan *Result)
	go func() {
		defer close(results)

		rows, err := store.db.Query("SELECT id, timestamp, client_ip, raw_request FROM results WHERE NOT EXISTS (SELECT NULL FROM parsed_results WHERE result = id)")
		if err != nil {
			log.Fatalf("error selecting results: %v", err)
		}
		for rows.Next() {
			var result Result
			if err := rows.Scan(&result.Id, &result.Timestamp, &result.RemoteAddr, &result.RawRequest); err != nil {
				log.Printf("error scanning result: %v", err)
			}
			results <- &result
		}
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows after selecting results: %v", err)
		}
	}()
	return results
}

func (store *postgresStore) WriteParsedResults(parsedResults <-chan *ParsedResult) {
	insertIntoResults, err := store.db.Prepare("INSERT INTO parsed_results (result, measurement_id, timestamp, outcome, message, origin, referer, client_ip, client_location, user_agent) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)")
	if err != nil {
		log.Fatalf("error preparing parsed_results insertion statement: %v", err)
	}
	defer insertIntoResults.Close()

	for parsedResult := range parsedResults {
		if _, err := insertIntoResults.Exec(parsedResult.Result, parsedResult.MeasurementId, parsedResult.Timestamp, parsedResult.Outcome, parsedResult.Message, parsedResult.Origin, parsedResult.Referer, parsedResult.ClientIp.String(), parsedResult.ClientLocation, parsedResult.UserAgent); err != nil {
			log.Printf("error inserting parsed result: %v", err)
		}
	}

	if err := insertIntoResults.Close(); err != nil {
		log.Printf("error while closing parsed results insert statement: %v", err)
	}
}

func (store *postgresStore) ComputeResultsTables() error {
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DROP TABLE IF EXISTS results_per_referer"); err != nil {
		return err
	}
	if _, err := tx.Exec("SELECT referer, count(distinct measurement_id) results INTO results_per_referer FROM parsed_results WHERE outcome = 'init' GROUP BY referer"); err != nil {
		return err
	}
	if _, err := tx.Exec("CREATE INDEX ON results_per_referer (referer)"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	tx, err = store.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DROP TABLE IF EXISTS results_per_day"); err != nil {
		return err
	}
	if _, err := tx.Exec(`SELECT referer, "timestamp"::date AS day, count(distinct measurement_id) results INTO results_per_day FROM parsed_results WHERE outcome = 'init' GROUP BY referer, timestamp::date`); err != nil {
		return err
	}
	if _, err := tx.Exec("CREATE INDEX ON results_per_day (referer)"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	tx, err = store.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DROP TABLE IF EXISTS results_per_country"); err != nil {
		return err
	}
	if _, err := tx.Exec("SELECT referer, client_location country, count(distinct measurement_id) results INTO results_per_country FROM parsed_results WHERE outcome = 'init' GROUP BY referer, client_location"); err != nil {
		return err
	}
	if _, err := tx.Exec("CREATE INDEX ON results_per_country (referer)"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	return nil
}

func (store *postgresStore) CountResultsForReferrer(requests <-chan CountResultsRequest) {
	query, err := store.db.Prepare("SELECT results FROM results_per_referer WHERE referer = $1")
	if err != nil {
		log.Fatalf("error preparing result count statement: %v", err)
	}

	for request := range requests {
		row := query.QueryRow(request.Referer)
		var count int
		if err := row.Scan(&count); err != nil {
			log.Printf("error scanning result count %s: %v", request.Referer, err)
			request.Response <- CountResultsResponse{
				Err: err,
			}
			continue
		}
		request.Response <- CountResultsResponse{
			Count: count,
			Err:   nil,
		}
	}

	if err := query.Close(); err != nil {
		log.Printf("error while closing results query: %v", err)
	}
}

func (store *postgresStore) ResultsPerDayForReferrer(requests <-chan ResultsPerDayRequest) {
	query, err := store.db.Prepare("SELECT day, results FROM results_per_day WHERE referer = $1 ORDER BY day")
	if err != nil {
		log.Fatalf("error preparing result count statement: %v", err)
	}

	for request := range requests {
		rows, err := query.Query(request.Referer)
		if err != nil {
			request.Response <- ResultsPerDayResponse{
				Err: err,
			}
			continue
		}
		results := make(map[string]int)
		var resultsErr error
		for rows.Next() {
			var day time.Time
			var count int
			if err := rows.Scan(&day, &count); err != nil {
				results = nil
				resultsErr = err
				break
			}
			results[day.Format("2006-01-02")] = count
		}
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows after querying results per day: %v", err)
		}

		if results == nil {
			request.Response <- ResultsPerDayResponse{
				Err: resultsErr,
			}
			continue
		}

		request.Response <- ResultsPerDayResponse{
			Results: results,
			Err:     err,
		}
	}

	if err := query.Close(); err != nil {
		log.Printf("error while closing results per day query: %v", err)
	}
}

func (store *postgresStore) ResultsPerCountryForReferrer(requests <-chan ResultsPerCountryRequest) {
	query, err := store.db.Prepare("SELECT country, results FROM results_per_country WHERE referer = $1 ORDER BY results DESC")
	if err != nil {
		log.Fatalf("error preparing result count statement: %v", err)
	}

	for request := range requests {
		rows, err := query.Query(request.Referer)
		if err != nil {
			request.Response <- ResultsPerCountryResponse{
				Err: err,
			}
			continue
		}
		results := make(map[string]int)
		for rows.Next() {
			var country string
			var count int
			if err := rows.Scan(&country, &count); err != nil {
				results = nil
				break
			}
			results[country] = count
		}
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows after selecting results per country: %v", err)
		}

		if results == nil {
			request.Response <- ResultsPerCountryResponse{
				Err: err,
			}
			continue
		}

		request.Response <- ResultsPerCountryResponse{
			Results: results,
			Err:     err,
		}
	}

	if err := query.Close(); err != nil {
		log.Printf("error while closing results per country query: %v", err)
	}
}
