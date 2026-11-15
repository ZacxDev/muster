package api

import "time"

// setTeamLiveEvery sets the membership sweep's interval on s (team_live.go):
// the seam the team stream tests shorten it through.
func setTeamLiveEvery(s *Server, d time.Duration) { s.teamLiveEvery = d }
