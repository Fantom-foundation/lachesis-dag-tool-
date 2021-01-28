package neo4j

import (
	"fmt"
	"time"

	"github.com/Fantom-foundation/go-lachesis/hash"
	"github.com/Fantom-foundation/go-lachesis/inter"
	"github.com/Fantom-foundation/go-lachesis/inter/idx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	lru "github.com/hashicorp/golang-lru"
	"github.com/neo4j/neo4j-go-driver/neo4j"
	"github.com/paulbellamy/ratecounter"
)

const (
	DefaultDb = "bolt://localhost:7687"

	// statsReportLimit is the time limit during import and export after which we
	// always print out progress. This avoids the user wondering what's going on.
	statsReportLimit = 8 * time.Second
)

type Db struct {
	drv   neo4j.Driver
	cache struct {
		EventsHeaders *lru.Cache
	}
}

func New(dbUrl string) (*Db, error) {
	db, err := neo4j.NewDriver(dbUrl, neo4j.NoAuth(), func(c *neo4j.Config) {
		c.Encrypted = false
	})
	if err != nil {
		return nil, err
	}

	session, err := db.Session(neo4j.AccessModeWrite)
	if err != nil {
		return nil, err
	}
	defer session.Close()

	DDLs := []string{
		"CREATE CONSTRAINT ON (e:Event) ASSERT e.id IS UNIQUE",
		"CREATE CONSTRAINT ON (e:Epoch) ASSERT e.id IS UNIQUE",
		"CREATE (e:Epoch {id:'current',num:1})",
	}
	for _, query := range DDLs {
		_, err = session.WriteTransaction(func(ctx neo4j.Transaction) (interface{}, error) {
			defer ctx.Close()

			err := exec(ctx, query)
			if err != nil {
				log.Warn("DDL", "err", err, "query", query)
				return nil, err
			}

			return nil, ctx.Commit()
		})
		if err != nil {
			ignoreFakeError(err)
		}
	}

	s := &Db{
		drv: db,
	}

	s.cache.EventsHeaders, err = lru.New(500)
	if err != nil {
		panic(err)
	}

	return s, nil
}

func (s *Db) Close() error {
	return s.drv.Close()
}

func (s *Db) HasEvent(e hash.Event) bool {
	// Get event from LRU cache first.
	if _, ok := s.cache.EventsHeaders.Get(e); ok {
		return true
	}

	session, err := s.drv.Session(neo4j.AccessModeRead)
	if err != nil {
		panic(err)
	}
	defer session.Close()

	id := eventID(e)

	res, err := session.ReadTransaction(func(ctx neo4j.Transaction) (interface{}, error) {
		res, err := search(ctx, `MATCH (e:Event %s) RETURN e`, fields{
			"id": id,
		})
		if err != nil {
			panic(err)
		}

		has := res.Next()
		return has, nil
	})
	if err != nil {
		ignoreFakeError(err)
	}

	return res.(bool)
}

func (s *Db) GetEvent(e hash.Event) *inter.EventHeaderData {
	// Get event from LRU cache first.
	if ev, ok := s.cache.EventsHeaders.Get(e); ok {
		return ev.(*inter.EventHeaderData)
	}

	session, err := s.drv.Session(neo4j.AccessModeRead)
	if err != nil {
		panic(err)
	}
	defer session.Close()

	id := eventID(e)

	res, err := session.ReadTransaction(func(ctx neo4j.Transaction) (interface{}, error) {
		res, err := search(ctx, `MATCH (e:Event %s) RETURN e.id as id, e.creator as creator`, fields{
			"id": id,
		})
		if err != nil {
			panic(err)
		}

		for res.Next() {
			ff := readFields(res.Record())
			header := new(inter.EventHeaderData)
			unmarshal(ff, header)
			return header, nil
		}
		return nil, nil
	})
	if err != nil {
		ignoreFakeError(err)
	}
	if res == nil {
		return nil
	}
	event := res.(*inter.EventHeaderData)

	res, err = session.ReadTransaction(func(ctx neo4j.Transaction) (interface{}, error) {
		res, err := search(ctx, `MATCH (e:Event %s)-[:PARENT]->(p) RETURN p.id`,
			fields{"id": id},
		)
		if err != nil {
			panic(err)
		}
		var parents hash.Events
		for res.Next() {
			p := eventHash(res.Record().GetByIndex(0).(string))
			parents = append(parents, p)
		}
		return parents, nil
	})
	if err != nil {
		ignoreFakeError(err)
	}
	event.Parents = res.(hash.Events)

	return event
}

// Load data from events chain.
func (s *Db) Load(events <-chan ToStore) {
	session, err := s.drv.Session(neo4j.AccessModeWrite)
	if err != nil {
		panic(err)
	}
	defer session.Close()
	// DML
	var (
		start    = time.Now().Add(-10 * time.Millisecond)
		reported time.Time
		counter  = ratecounter.NewRateCounter(60 * time.Second).WithResolution(1)
		total    int64
		last     hash.Event
	)
	for task := range events {
		event := task.Payload()
		id := eventID(event.Hash())
		_, err = session.WriteTransaction(func(ctx neo4j.Transaction) (interface{}, error) {
			defer ctx.Close()

			data := marshal(event)
			log.Debug("<<<", "event", event.Hash(), "data", data, "parents", event.Parents)
			err = exec(ctx, "CREATE (e:Event %s)", data)
			if err != nil {
				panic(err)
			}

			for _, p := range event.Parents {
				err = exec(ctx, `MATCH (e:Event %s), (p:Event %s) CREATE (e)-[:PARENT]->(p)`,
					fields{"id": id},
					fields{"id": eventID(p)},
				)
				if err != nil {
					panic(err)
				}
			}

			return nil, ctx.Commit()
		})
		if err != nil {
			ignoreFakeError(err)
		}

		s.cache.EventsHeaders.Add(event.Hash(), event)
		task.Done()

		counter.Incr(1)
		total++
		last = event.Hash()
		if time.Since(reported) >= statsReportLimit {
			log.Info("<<<",
				"last", last,
				"rate", counter.Rate()/60,
				"total", total,
				"elapsed", common.PrettyDuration(time.Since(start)))
			reported = time.Now()
		}
	}

	log.Info("Total imported events",
		"last", last,
		"rate", total*1000/time.Since(start).Milliseconds(),
		"total", total,
		"elapsed", common.PrettyDuration(time.Since(start)))
}

// FindAncestors of event.
func (s *Db) FindAncestors(e hash.Event) []hash.Event {
	session, err := s.drv.Session(neo4j.AccessModeRead)
	if err != nil {
		panic(err)
	}
	defer session.Close()

	id := eventID(e)

	res, err := session.ReadTransaction(func(ctx neo4j.Transaction) (interface{}, error) {
		res, err := search(ctx, "MATCH (p:Event %s)-[:PARENT*]->(s:Event) RETURN DISTINCT s.id", fields{
			"id": id,
		})
		if err != nil {
			panic(err)
		}

		var ancestors []hash.Event
		for res.Next() {
			pid := eventHash(res.Record().GetByIndex(0).(string))
			ancestors = append(ancestors, pid)
		}
		return ancestors, nil
	})
	if err != nil {
		ignoreFakeError(err)
	}

	return res.([]hash.Event)
}

func (s *Db) SetEpoch(num idx.Epoch) {
	const key = "current"
	session, err := s.drv.Session(neo4j.AccessModeWrite)
	if err != nil {
		panic(err)
	}
	defer session.Close()

	_, err = session.WriteTransaction(func(ctx neo4j.Transaction) (interface{}, error) {
		defer ctx.Close()

		err := exec(ctx, `MATCH (e:Epoch %s) SET e.num = %d`,
			fields{"id": key}, num)
		if err != nil {
			panic(err)
		}

		return nil, ctx.Commit()
	})
	if err != nil {
		ignoreFakeError(err)
	}
}

func (s *Db) GetEpoch() idx.Epoch {
	const key = "current"

	session, err := s.drv.Session(neo4j.AccessModeRead)
	if err != nil {
		panic(err)
	}
	defer session.Close()

	res, err := session.ReadTransaction(func(ctx neo4j.Transaction) (interface{}, error) {
		res, err := search(ctx, `MATCH (e:Epoch %s) RETURN e.num as num`, fields{
			"id": key,
		})
		if err != nil {
			panic(err)
		}

		for res.Next() {
			epoch := idx.Epoch(res.Record().GetByIndex(0).(int64))
			return epoch, nil
		}
		return nil, nil
	})
	if err != nil {
		ignoreFakeError(err)
	}
	if res == nil {
		return idx.Epoch(1)
	}
	return res.(idx.Epoch)
}

func exec(ctx neo4j.Transaction, cypher string, a ...interface{}) error {
	query := fmt.Sprintf(cypher, a...)
	log.Debug("cypher", "query", query)
	_, err := ctx.Run(query, nil)
	if err != nil {
		return err
	}

	return nil
}

func search(ctx neo4j.Transaction, cypher string, a ...interface{}) (neo4j.Result, error) {
	query := fmt.Sprintf(cypher, a...)
	log.Debug("cypher", "query", query)
	res, err := ctx.Run(query, nil)
	if err != nil {
		return nil, err
	}

	return res, nil
}

func ignoreFakeError(err error) {
	log.Trace("neo4j non critical error", "err", err)
}
