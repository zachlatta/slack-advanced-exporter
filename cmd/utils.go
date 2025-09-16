package cmd

import "log"

// verbosePrintln writes to stdout when --verbose is set.
func verbosePrintln(line string) {
	if verbose {
		println(line)
	}
}

// verboseLogf writes formatted logs to stderr when --verbose is set.
func verboseLogf(format string, args ...interface{}) {
	if verbose {
		log.Printf(format, args...)
	}
}
