package cli

import (
	"fmt"
	"io"

	"github.com/ezgamehost/ezdbbackup/internal/backup"
	"github.com/ezgamehost/ezdbbackup/internal/terminal"
)

func encoded(value any) string {
	return terminal.Encode(fmt.Sprint(value))
}

func printProgress(writer io.Writer, event backup.ProgressEvent) {
	job := encoded(event.Job)
	switch event.Kind {
	case backup.ProgressStarted:
		fmt.Fprintf(writer, "%s: starting backup\n", job)
	case backup.ProgressStaged:
		fmt.Fprintf(writer, "%s: staged %d bytes\n", job, event.Size)
	case backup.ProgressUploadStarted:
		fmt.Fprintf(writer, "%s: uploading %s\n", job, encoded(event.ObjectKey))
	case backup.ProgressCompleted:
		fmt.Fprintf(writer, "%s: upload complete %s (%d bytes)\n", job, encoded(event.ObjectKey), event.Size)
	case backup.ProgressFailed:
		fmt.Fprintf(writer, "%s: failed at %s: %s\n", job, encoded(event.Stage), encoded(event.Error))
	}
}
