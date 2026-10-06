SKILLS_DIR ?= $(HOME)/.claude/skills
SKILL := $(SKILLS_DIR)/spam-intake

.PHONY: install-skill

# install-skill: link the spam-intake Claude Code skill into ~/.claude/skills,
# so it follows this clone when it is updated.
install-skill:
	@if [ -e "$(SKILL)" ] && [ ! -L "$(SKILL)" ]; then \
		echo "$(SKILL) exists and is not a symlink; move it aside first" >&2; exit 1; fi
	mkdir -p "$(SKILLS_DIR)"
	ln -sfn "$(CURDIR)/.claude/skills/spam-intake" "$(SKILL)"
	@echo "spam-intake -> $$(readlink -f "$(SKILL)")"
