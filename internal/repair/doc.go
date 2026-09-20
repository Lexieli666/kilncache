// Package repair implements the bounded background worker pool that audits
// locally held objects, detects retained objects whose second replica is
// missing, and recreates the missing copy.
package repair
