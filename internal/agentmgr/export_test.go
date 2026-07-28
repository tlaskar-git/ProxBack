package agentmgr

// NoteProgressForTest stands in for the chunk uploads that mark a live run.
// Compiled only under `go test`, so the production API keeps no test hook.
func (m *Manager) NoteProgressForTest(runID string) { m.noteProgress(runID) }
