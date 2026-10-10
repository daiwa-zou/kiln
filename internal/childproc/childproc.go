// Package childproc holds what every subprocess kiln starts has in common.
//
// Several of them (git, pdftotext, pandoc, the agent CLI) parse content kiln
// does not control, so the limits that keep one hostile or broken input from
// hurting the worker live in one place rather than being re-derived, and
// forgotten, at each exec site.
package childproc

import "time"

// WaitDelay is how long a command may keep running, after its context ends,
// before Wait gives up on it and closes its pipes.
//
// exec.CommandContext kills only the direct child. When stdout and stderr are
// captured into buffers, Wait also blocks until every holder of the other end
// of those pipes has exited, so a grandchild (git clone spawns
// git-remote-https; a converter may fork a helper) that outlives the killed
// child pins Wait, and with it the caller, long past the timeout the caller
// asked for. WaitDelay turns the timeout into a real wall-clock bound.
const WaitDelay = 10 * time.Second
