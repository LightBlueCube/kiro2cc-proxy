package sysprompt

// DefaultSignatures list each harness's name and the identity lines of its
// prompts. Upstream refuses by substring (measured: "You are opencode" and
// "You are Claude Code" in any case and spacing, "You are opencodes" too),
// so a name is listed bare as a phrase when it is no common word and no
// part of one. OpenCode, Zed, Roo, Cline, Trae, pi and omp appear inside
// other words or in paths (optimized, root, decline, Traefik, api, compile,
// /opencode/), so they are Words. Amp, Cursor, Crush, Goose,
// Augment and Kilo are common words themselves; they are not listed bare.
var DefaultSignatures = []Signature{
	{Name: "OpenCode", Phrases: []string{
		"opencode.",
		"You are opencode",
		"Your name is opencode",
		"You are an AI agent running in OpenCode, a coding agent harness",
		"You are an AI agent powered by OpenCode, a coding agent harness",
		"users should report the issue at https://github.com/anomalyco/opencode",
	}, Words: []string{
		"OpenCode",
		"OpenCodes",
		"OpenCodex",
	}},
	{Name: "Kilo Code", Phrases: []string{
		"Kilo Code",
		"You are Kilo",
		"You are Kilo Code",
		"users should report the issue at https://github.com/Kilo-Org/kilocode",
	}},
	{Name: "Claude Code", Phrases: []string{
		"Claude Code",
		"Anthropic's official CLI for Claude",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK",
		"You are running inside the Claude desktop app",
	}},
	{Name: "Codex", Phrases: []string{
		"Codex",
		"You are Codex, ",
		"You are a coding agent running in the Codex CLI",
		"Codex CLI is an open source project led by OpenAI",
		"You are running as a coding agent in the Codex CLI",
		"the primary agent in a team of agents collaborating to fulfill the user's goals",
	}},
	{Name: "GitHub Copilot", Phrases: []string{
		"Copilot",
		"Your name is GitHub Copilot",
		"you must respond with \"GitHub Copilot\"",
		"You are an expert AI programming assistant, working with a user in the VS Code editor",
		"You are a highly sophisticated automated coding agent with expert-level knowledge across many different programming languages and frameworks",
	}},
	{Name: "Gemini CLI", Phrases: []string{
		"Gemini CLI",
		"You are Gemini CLI",
	}},
	{Name: "Qwen Code", Phrases: []string{
		"Qwen Code",
		"You are Qwen Code",
	}},
	{Name: "Cline", Phrases: []string{
		"You are Cline",
	}, Words: []string{
		"Cline",
	}},
	{Name: "Roo Code", Phrases: []string{
		"You are Roo",
	}, Words: []string{
		"Roo",
	}},
	{Name: "Kimi CLI", Phrases: []string{
		"Kimi CLI",
		"Kimi Code CLI",
		"You are Kimi Code CLI",
	}},
	{Name: "Crush", Phrases: []string{
		"You are Crush",
	}},
	{Name: "Goose", Phrases: []string{
		"AI agent called goose",
		"goose is being developed as an open-source software project",
	}},
	{Name: "Aider", Phrases: []string{
		"Aider",
		"Take requests for changes to the supplied code",
		"Act as an expert architect engineer and provide direction to your editor engineer",
		"Act as an expert code analyst. Answer questions about the supplied code",
		"Respect and use existing conventions, libraries, etc that are already present in the code base",
	}},
	{Name: "OpenHands", Phrases: []string{
		"OpenHands",
		"You are OpenHands agent",
	}},
	{Name: "OpenClaw", Phrases: []string{
		"OpenClaw",
		"You are a personal assistant running inside OpenClaw",
	}},
	{Name: "Hermes Agent", Phrases: []string{
		"Hermes Agent",
		"You are Hermes Agent",
		"You run on Hermes Agent",
	}},
	{Name: "Grok CLI", Phrases: []string{
		"Grok CLI",
		"You are Grok released by xAI",
	}},
	{Name: "pi", Phrases: []string{
		"pi-coding-agent",
		"operating inside pi, a coding agent harness",
	}, Words: []string{
		"pi",
	}},
	{Name: "oh-my-pi", Phrases: []string{
		"oh my pi",
		"Oh My Pi coding harness",
		"You are omp's trusted coding assistant",
	}, Words: []string{
		"omp",
	}},
	{Name: "MiMo Code", Phrases: []string{
		"MiMo Code",
		"MiMoCode",
		"You are MiMoCode",
	}},
	{Name: "ZCode", Phrases: []string{
		"ZCode",
		"You are ZCode",
		"You are an interactive ZCode agent",
	}},
	{Name: "DeepSeek Harness", Phrases: []string{
		"DeepSeek Harness",
		"powered by DeepSeek Harness",
	}},
	{Name: "Agent Zero", Phrases: []string{
		"Agent Zero",
		"Agent Zero System Manual",
		"agent zero autonomous json ai agent",
	}},
	{Name: "OpenSquilla", Phrases: []string{
		"OpenSquilla",
		"You are OpenSquilla",
	}},
	{Name: "Zed", Phrases: []string{
		"You are the Zed coding agent",
	}, Words: []string{
		"Zed",
	}},
	{Name: "Continue", Phrases: []string{
		"Always include the language and file name in the info string when you write code blocks",
	}},
	{Name: "Void", Phrases: []string{
		"Here is an overview of the user's file system",
	}},
	{Name: "Windsurf", Phrases: []string{
		"You are Cascade",
		"designed by the Windsurf engineering team",
	}},
	{Name: "Trae", Phrases: []string{
		"You are Trae AI",
		"You operate exclusively in Trae AI",
	}, Words: []string{
		"Trae",
	}},
	{Name: "Cursor", Phrases: []string{
		"You operate in Cursor",
		"Bias towards not asking the user for help if you can find the answer yourself",
	}},
	{Name: "Kiro", Phrases: []string{
		"Kiro",
		"You are Kiro, an AI assistant and IDE built to assist developers",
	}},
	{Name: "Amp", Phrases: []string{
		"You are Amp",
	}},
	{Name: "Augment Code", Phrases: []string{
		"Augment Code",
		"You are Augment Agent developed by Augment Code",
	}},
	{Name: "Junie", Phrases: []string{
		"Junie",
		"Your name is Junie",
	}},
	{Name: "Warp", Phrases: []string{
		"an AI agent running within Warp",
	}},
	{Name: "CodeBuddy", Phrases: []string{
		"CodeBuddy",
		"/help: Get help with using CodeBuddy Code",
		"users should report the issue at https://cnb.cool/codebuddy/codebuddy-code",
		"You are CodeBuddy, a highly skilled software engineer",
	}},
}
