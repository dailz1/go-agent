// Package snapshot durably prepares edit/write images before one application
// and restores the latest change per path with conflict checks.
// Shell effects are never represented as recoverable file edits. The host owns
// approval, the workspace process lock, and the idle restore operation slot.
package snapshot
