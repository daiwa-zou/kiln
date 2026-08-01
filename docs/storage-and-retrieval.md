# Storage and retrieval: is Postgres still the right answer?

Prompted by exposing the wiki to agents over MCP. The question as usually asked —
*"is Postgres the right database now that agents read it?"* — bundles two
separate questions with different answers:

1. **Is Postgres the right primary store?** Yes, and more so than before. Nothing
   here argues for replacing it.
2. **Is Postgres full-text search the right retrieval for agent queries?** No.
   It is measurably brittle for the questions agents actually ask, and that is a
   query-construction and ranking problem, not a storage-engine problem.

Conflating the two is how teams end up migrating a database to fix a search
relevance bug.

---

## 1 · Postgres as the primary store: keep it

Four properties of the design are implemented *in* Postgres rather than on top
of it. Each would have to be rebuilt, worse, somewhere else.

| Property | How Postgres provides it | Cost of moving |
| --- | --- | --- |
| **Atomic import** | One transaction over pages, links, sources, and artifacts, serialized per workspace by `pg_advisory_xact_lock` | A document store needs application-level coordination and a reconciliation story for partial writes |
| **The build queue** | The `runs` table itself, claimed with `FOR UPDATE SKIP LOCKED` — claim and state change in one statement | An external broker, plus the exactly-once problem the single statement currently makes impossible |
| **Deletion cascade** | Reference-counting pages across source records, relationally | Hand-rolled graph bookkeeping, with orphan and double-delete failure modes |
| **Multi-tenancy** | Foreign keys and per-query org/workspace scoping | Enforcement moves entirely into application code, where a missed scope is a data leak |

The queue is the clearest case. An external broker was rejected deliberately —
LLM-bound jobs at runs-per-hour throughput gain nothing from one, and would lose
the single-transaction claim this gets for free. That reasoning is unchanged by
MCP, which adds *read* traffic only.

**MCP does not pressure the store.** `kiln mcp` reads through the HTTP API, so
agent traffic is the same queries the UI issues. Reads scale the usual way
(replicas), and the API already sends an ETag derived from the workspace
revision, bumped inside the import transaction — so a polling agent gets `304`s
until something actually changes.

> **Gap found while reviewing this:** `handleSearch` is the only read handler
> that does *not* call `notModified`. It is simultaneously the most expensive
> query and the one agents hit most. Search results are a pure function of
> `(workspace revision, query)`, so they are exactly as cacheable as the page
> list. Worth fixing regardless of anything else here.

---

## 2 · Retrieval for agents: the real problem

kiln searches with `plainto_tsquery`, which **ANDs every term**:

```sql
plainto_tsquery('english', 'what happens when a worker dies')
  -- 'happen' & 'worker' & 'die'
```

Every one of those lexemes must appear in a page for it to match at all. Humans
type keywords and tolerate this. Agents ask questions.

### Measured

A corpus of the 20 sections of [architecture.md](architecture.md) — real prose
about this exact system — indexed exactly as `pages.search` is, then queried the
way an agent would:

| Query | `plainto_tsquery` (current) | OR-ranked |
| --- | --- | --- |
| how is spending controlled | Cost control | Cost control |
| why did my run cost nothing | The idea; The hash gate | — |
| can two units write the same page | Sources and connectors | — |
| **what happens when a worker dies** | **no match** | Failure and recovery ✓ |
| **what stops a runaway bill** | **no match** | Failure and recovery ✗ |
| **how do I make builds faster** | **no match** | How a build is triggered ✗ |
| budget *(keyword)* | Generation; Validation | — |
| concurrency *(keyword)* | Generation; Topology | — |

Three of eight natural-language queries return **nothing at all**. Single
keywords work fine. The failure mode is precisely agent-shaped input.

The worker case shows the mechanism exactly. The Failure-and-recovery section
contains the literal sentence *"Worker dies mid-build"*. The query contributes
three lexemes; `worker` and `die` are both present, `happen` is not — and the
AND kills a perfect match on one incidental word from the question.

*(Caveat: 20 sections of one document is a small sample, chosen because it is
real prose about this system rather than fixtures. It is enough to show the
failure mode is systematic, not enough to quantify a hit rate.)*

### The ceiling of the cheap fix

OR-ing the lexemes and letting `ts_rank` sort them converts all three hard
failures into answers, one of them exactly right. That alone is a large win: an
agent handed a plausible page can read it and follow links, whereas an agent
handed nothing gives up or invents.

But two of the three land on the wrong section, and the reason is not fixable
lexically. *"Runaway bill"* shares no stem with *budget* or *ceiling*.
*"Faster"* shares none with *concurrency*. Answering those needs meaning, not
tokens.

---

## 3 · Options, priced

| Option | Buys | Costs | Verdict |
| --- | --- | --- | --- |
| **A · OR + rank** | Eliminates empty results on NL queries. Pure query change, ~20 lines, no migration, no new dependency | Slightly noisier top-N for keyword queries | **Do first** |
| **B · `pg_trgm` fuzzy fallback** | Typo and morphology tolerance; already available in `postgres:16-alpine` | Another index; still lexical | Cheap, optional |
| **C · `pgvector` hybrid** | Actual semantic retrieval. Reciprocal-rank fusion of FTS + vector is the current sweet spot | Embedding cost and pipeline stage; **image change** — pgvector is *not* in `postgres:16-alpine` | **The real answer, when B stops sufficing** |
| **D · External vector DB** | Nothing C does not, at this scale | A second datastore to run, back up, and keep consistent with Postgres | **No** |

**On D specifically.** kiln's operational promise is one binary plus Postgres.
A bench is one repository or document set — hundreds to low thousands of pages;
an instance maybe 10⁵. pgvector with an HNSW index is comfortable into the
millions. Adding a dedicated vector database buys nothing at this scale and
costs the property that makes kiln easy to run. Revisit only if a single
instance passes ~10⁷ embedded chunks, which is several orders of magnitude away.

**On C's real cost.** The blocker is not pgvector, it is that embeddings need
generating and regenerating. That work belongs where regeneration is already
decided: the same content hash that gates a page's prose can gate its embedding,
so an unchanged page costs nothing to re-embed — the same property the whole
pipeline is built on. Managed Postgres (RDS, Cloud SQL, Azure) all support
pgvector; the change is the image in `docker-compose.yml` and the Helm chart.

---

## Recommendation

**Keep Postgres. Fix retrieval, in three steps, stopping when it is good enough.**

1. **OR + rank, and an ETag on search — done.** The AND query now decides
   *order* and the OR query decides *membership*, so a page matching every term
   still ranks above one matching some: keyword search returns exactly what it
   did, and question-shaped queries continue past where they used to stop dead.
   Search also sends an ETag now, which it alone among the read handlers did
   not. No migration, no dependency, no image change.
2. **Measure — done.** `kiln_searches_total{outcome="hit"|"empty"}` counts the
   empty rate without recording what anyone asked. Deliberately a counter with
   a coarse label rather than a query log: the rate is the decision input, the
   text is a privacy liability. Only first pages are counted, so paging past
   the end of a result set does not inflate it.
3. **When the data says so — pgvector hybrid.** Embed page bodies at import,
   gated by the same content hash that gates generation; fuse FTS and vector
   ranks. Stays inside Postgres and inside the existing cost model.

Step 3 is deliberately not built yet. It is the largest of the three and the
only one that cannot be justified from first principles — the empty rate from
step 2 is what says whether it is needed, and shipping an embedding pipeline
before that number exists would be guessing expensively.

Do not migrate the primary store. The properties the design leans on — atomic
import, the queue, the cascade, tenancy — are the ones a different engine would
make harder, and none of them are what MCP strains.

---

## What would change this answer

Honest triggers for revisiting, rather than a claim that Postgres is right forever:

- **A single instance passing ~10⁷ embedded chunks.** pgvector's HNSW build time
  and memory become an operational concern; a dedicated vector store starts
  earning its keep.
- **Read traffic outgrowing replicas.** Not near: ETags make polling nearly free,
  and agent reads are the same cheap queries the UI makes.
- **Cross-bench semantic search as a product feature.** Searching every bench at
  once is a different query shape than searching one, and would want its own
  index. Still pgvector, but a different table.
- **Write throughput.** Would only arrive if kiln moved from runs-per-hour builds
  to something continuous, which would change much more than the database.
