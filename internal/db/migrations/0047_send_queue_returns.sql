-- muster 0047 — A QUEUED MESSAGE A USAGE LIMIT REJECTED GOES BACK TO ITS QUEUE.
--
-- When Claude Code hits a usage limit it rejects the message with a record it
-- writes itself and goes back to its prompt; the send queue used to read that
-- as a finished turn and send the next message into the same limit, until the
-- queue was empty. Now it holds (internal/sendqueue limit.go), and a message
-- the transcript shows was rejected UNPROCESSED is put back at the head of its
-- queue (PGStore.Return).
--
-- returns counts how many times that happened to a row: the row is drawn
-- "returned: usage limit", and each send of it is a new delivery whose nonce
-- names the attempt (internal/api sendQueueNonce), so the send path's repeat
-- guard does not take the re-send for the delivery the limit refused.
SET LOCAL lock_timeout = '3s';

ALTER TABLE public.send_queue ADD COLUMN IF NOT EXISTS returns INTEGER NOT NULL DEFAULT 0;

ALTER TABLE public.send_queue DROP CONSTRAINT IF EXISTS send_queue_returns_check;
ALTER TABLE public.send_queue ADD CONSTRAINT send_queue_returns_check CHECK (returns >= 0);
