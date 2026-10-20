`legacy-mattn.db` is a synthetic compatibility fixture created using
`github.com/mattn/go-sqlite3 v1.14.52` with the state schema and one fabricated
message/delivery. It contains no credentials or real email. Its timestamp uses
the legacy SQLite CURRENT_TIMESTAMP format. The migration test copies it into
a temporary directory, preserving the original fixture.
