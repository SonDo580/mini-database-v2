# Mini Database

A relational database engine over B+tree

## Guide

https://build-your-own.org/database/

## Execution pipeline

[QL] -> [QL Parser] -> [QL Executor] -> [Table & Index] -> [KV Store] -> [B+Tree]

## Some techniques

**1. Copy-on-write for B+tree operations**

- Insertion/deletion starts at leaf node.
- After making a copy with modification, parent node is updated to point to new node, which is also done on its copy.
- The copying propagates to root node, resulting in a new tree root.
- The original tree remains intact and accessible from the old root.

**2. Optimistic concurrency control**

- Don't lock rows/tables **(pessimistic concurrency control)**.
  Just abort the transaction when a conflict is detected.
- **Phases**:
  - Transaction starts.
  - Reads are performed on snapshot. Writes are buffered.
  - Before committing, check for conflicts with committed transactions.
  - Transaction ends:
    - If there're conflicts, abort and rollback.
    - Otherwise, transfer buffered writes to DB.
- **Detect conflicts**:
  - current transaction attempted updates & read key ranges overlap with write key ranges of a committed newer-version transaction _(even if no changes happened, that "no-changes" result depends on the stale state of the dependency)_.

## Self-implemented

- **Improvements:**
  - binary search for key in B+tree node.
  - short-circuit evaluation (`AND`, `OR`).
  - string representations for expressions.

- **Extensions**:
  - add **transaction control statements** to query language _(underlying engine already supports the logic)_: `BEGIN`, `COMMIT`, `ROLLBACK`
  - **auto-commit** mode: if a statement is not inside an explicit transaction block, automatically create a transaction to execute it.

- **Modifications to query language:**
  - disallow trailing comma.
  - `count` is size limit, not end offset.
  - `offset` and `count` are applied to rows matching `filter`, not all rows matching `index by`.

## Query language specification

Similar but not exactly `SQL`. The following is not official grammar, just descriptions and examples of some key parts.

- **Statements**:

```sql
create table table_name (
a type1,
b type2,
...
index (c, b, a),
...
primary key (a, b)
);

select expr... from table_name <conditions> <limit>;

insert into table_name (cols...) values (a, b, c)...;
replace into table_name (cols...) values (a, b, c)...;
upsert into table_name (cols...) values (a, b, c)...;

delete from table_name <conditions> <limit>;

update table_name set a = expr, b = expr, ... <conditions> <limit>;

-- transaction control (my extension)
begin
commit
rollback
```

- **Conditions**:
  - don't use `WHERE`, use separate clauses for indexing and filtering (`INDEX BY` and `FILTER`).
  - **both are optional**; primary key is used if `INDEX BY` is missing.
  - we make the index selection explicit to save the DB from guessing.

```sql
-- INDEX BY: 2 forms
index by cols <cmp> vals
index by cols1 <cmp1> vals1 AND cols1 <cmp2> vals1
-- cmp: comparison operators, except '!=' ('=' can only be use in the 1st form)
-- cols, vals: must be 2 non-tuple items, or 2 tuples with the same number of items
-- cols: must contain only symbols (column names)

-- INDEX BY examples
select expr... from table_name index by a = 1;
select expr... from table_name index by a > 1;
select expr... from table_name index by a > 1 and a < 5;
select expr... from table_name index by a < 5 and a > 1; -- descending order


-- FILTER is any expression that evaluates to boolean or int
select expr... from table_name index by condition1 filter condition2;
select expr... from table_name filter condition;
```

- **Limit (optional)**:

```sql
-- 2 forms
select expr... from table_name limit count;
select expr... from table_name limit offset, count;
```

- **Expressions**:

```sql
'...', "..." -- string literal
123 -- (positive) int64 literal
(a, b, c), (1, "s", 's') -- tuple
a -- symbol (column name)
-- no quoted symbol (example: CREATE TABLE "select" ...)

-- unary
NOT a
-a

-- binary
a = b, a != b, a < b, a <= b, a > b, a >= b -- comparison
a OR b, a AND b -- logic
a + b, a - b, a * b, a / b, a % b -- arithmetic

* -- select *
```

- **Column types**:

```sql
a int, a int64 -- int64
a string, a bytes -- string
```
