// Package snapshot will preserve edit/write before and after images.
// TODO(C): make prepared/applied/restored state durable before exposing writes.
// Shell effects are never represented as recoverable file edits.
package snapshot
