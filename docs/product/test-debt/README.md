# Test debt

When the owner merges a phase before its Test Report is `PASS` and its Review
Report is `APPROVE` on the same head, everything still unproven is written
here, one file per phase. Test debt is a known trade-off, not a pass: no PCC ID
covered only by indebted code may become `VERIFIED` in `TRACEABILITY.md`.

## Rules

- The file names the merged head, what was proven on which head, and every
  unproven item with its REQ and PCC IDs.
- The next Phase Brief lists the open debt files. The Tester runs the debt
  items first (on the new head) and reports them in a separate section of its
  Test Report; the Reviewer reviews the indebted commits.
- An item is closed only by a Test Report or Review Report that proves it;
  the Lead marks it closed with the report number and head.
- A defect found while paying debt is fixed like any other defect, in the
  current phase's Fix Brief.
- When every item in a file is closed, the Lead moves the file to
  `closed/`.
