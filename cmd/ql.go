package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/SonDo580/mini-database-v2/db"
	"golang.org/x/term"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <db_path>\n", os.Args[0])
		os.Exit(1)
	}
	dbPath := os.Args[1]

	database := &db.DB{Path: dbPath}
	err := database.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
	}

	repl(database)
}

func repl(database *db.DB) {
	fmt.Println("QL version 0.0.1")
	fmt.Println("Enter \".quit\" to exit this program")

	// enable raw mode; restore terminal state when REPL exits
	termState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		panic(err)
	}
	defer term.Restore(int(os.Stdin.Fd()), termState)
	terminal := term.NewTerminal(os.Stdin, "ql> ")

	stmtScanner := db.NewStmtScanner()
	executor := db.NewExecutor(database)

	stmtIncomplete := false
	inTransaction := false
	stmtStrsBuf := [][]byte{} // buffer statements until input ends with a complete statement

	for {
		// dynamic prompt
		prompt := getPrompt(stmtIncomplete, inTransaction)
		terminal.SetPrompt(prompt)

		// read next input line
		line, err := terminal.ReadLine()
		if err != nil {
			if err == io.EOF {
				break
			}
			panic("unreachable")
		}

		// handle exit command
		if !stmtIncomplete && strings.TrimSpace(line) == ".quit" {
			break
		}

		stmtScanner.Append([]byte(line))

		// scan all valid statements
		for {
			stmtStr, err := stmtScanner.NextStmtStr()

			if errors.Is(err, db.ErrStmtIncomplete) {
				stmtIncomplete = true
				break // continue reading to get complete statement
			}

			if errors.Is(err, db.ErrStmtEmpty) {
				continue // no-op
			}

			if stmtStr == nil { // input ends with a complete statement
				stmtIncomplete = false
				break // to start execution
			}

			stmtStrsBuf = append(stmtStrsBuf, stmtStr)
		}

		if stmtIncomplete {
			continue // skip execution, continue reading
		}

		// execute buffered statements
		for _, stmtStr := range stmtStrsBuf {
			res, inTX, err := executor.ExecStr(stmtStr)
			inTransaction = inTX
			if err != nil {
				fmt.Fprintf(terminal, "Execution error: %v\r\n", err)
				break // skip remaining statements, read next input
			}

			err = printResult(terminal, res) // show result
			if err != nil {
				fmt.Fprintf(terminal, "Show result error: %v\r\n", err)
				break // skip remaining statements, read next input
			}
		}

		// reset scanner and buffer
		stmtScanner.Reset()
		stmtStrsBuf = stmtStrsBuf[:0]
	}
}

func getPrompt(stmtIncomplete, inTransaction bool) string {
	if inTransaction {
		if stmtIncomplete {
			return "(tx)...> "
		}
		return "ql (tx)> "
	} else {
		if stmtIncomplete {
			return "...> "
		} else {
			return "ql> "
		}
	}
}

func printResult(w io.Writer, res db.QLResult) error {
	if res.Records != nil {
		return printRecords(w, res.Records)
	} else {
		printStats(w, res)
		return nil
	}
}

func printStats(w io.Writer, res db.QLResult) {
	assert(res.Deleted*res.Updated == 0)
	assert(res.Updated >= res.Added)
	modified := res.Updated - res.Added

	if res.Deleted > 0 {
		fmt.Fprintf(w, "Deleted %d rows\r\n", res.Deleted)
	}
	if res.Added > 0 {
		fmt.Fprintf(w, "Inserted %d rows\r\n", res.Added)
	}
	if modified > 0 {
		fmt.Fprintf(w, "Modified %d rows\r\n", modified)
	}
}

// print records with "line" mode:
//   - line format: `<col>: <val>`.
//   - records are separated by blank line.
func printRecords(w io.Writer, iter db.RecordIter) error {
	for ; iter.Valid(); iter.Next() {
		rec := &db.Record{}
		err := iter.Deref(rec)
		if err != nil {
			return err
		}
		printRecord(w, rec)
		fmt.Fprintf(w, "\r\n")
	}
	return nil
}

func printRecord(w io.Writer, rec *db.Record) {
	assert(len(rec.Cols) == len(rec.Vals))
	for i := range len(rec.Cols) {
		fmt.Fprintf(w, "%s: %s\r\n", rec.Cols[i], rec.Vals[i].Display())
	}
}

func assert(cond bool) {
	if !cond {
		panic("assertion failure")
	}
}
