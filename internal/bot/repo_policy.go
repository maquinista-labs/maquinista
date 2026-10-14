package bot

import (
	"context"
	"fmt"
	"strconv"

	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/routing"
)

// repoPolicyFromConfig builds the routing.RepoPolicy enforced across the
// ladder from the per-user repo bindings in cfg (MAQ-45, USER_REPOS).
// Users without a binding pass everywhere — the pre-MAQ-45
// single-operator behavior; bound users may only reach agents rooted in
// one of their repos, and the check fails closed on unresolvable roots.
func repoPolicyFromConfig(cfg *config.Config) routing.RepoPolicy {
	return func(ctx context.Context, userID, repoRoot string) error {
		id, err := strconv.ParseInt(userID, 10, 64)
		if err != nil {
			return fmt.Errorf("%w: non-numeric user %q", routing.ErrRepoForbidden, userID)
		}
		if !cfg.MayAccessRepo(id, repoRoot) {
			return fmt.Errorf("%w: user %d may not access repo %q", routing.ErrRepoForbidden, id, repoRoot)
		}
		return nil
	}
}

// repoForbiddenText is the user-facing routing rejection for a cross-user
// repo access attempt. Deliberately plain: the operator knows the mapping,
// the pilot user just needs to know the target is out of bounds.
const repoForbiddenText = "This agent belongs to a project outside your allowed repos — you can only interact with agents for your own project."
