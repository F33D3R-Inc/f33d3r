package db

import "time"

// heraldTimeout bounds a read of the notification inbox, so a slow Herald delays a
// page by this much at most.
const heraldTimeout = 3 * time.Second
