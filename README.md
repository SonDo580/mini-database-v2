# Mini Database

A relational database engine over B+tree

## Guide

https://build-your-own.org/database/

## My extensions

- **Improvements:**
  - binary search for key in B+tree node.
  - short-circuit evaluation (`AND`, `OR`).
  - string representations for expressions.

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
