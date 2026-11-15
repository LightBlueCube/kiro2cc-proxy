package sysprompt

// DefaultSignatures were chosen against a corpus of the harnesses' captured
// and published prompts (see TestCorpus): every phrase occurs in its
// harness's default prompts and in no other harness's, nor in a side
// request, so their order does not matter.
var DefaultSignatures = []Signature{
	{Name: "OpenCode", Phrases: []string{
		"You are opencode,",
		"Your name is opencode",
		"You are an AI agent running in OpenCode, a coding agent harness",
		"users should report the issue at https://github.com/anomalyco/opencode",
	}},
	{Name: "Kilo Code", Phrases: []string{
		"You are Kilo,",
		"You are Kilo Code,",
		"users should report the issue at https://github.com/Kilo-Org/kilocode",
	}},
	{Name: "Claude Code", Phrases: []string{
		"Anthropic's official CLI for Claude",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK",
		"You are running inside the Claude desktop app",
	}},
	{Name: "Codex", Phrases: []string{
		"You are Codex, ",
		"You are a coding agent running in the Codex CLI",
		"Codex CLI is an open source project led by OpenAI",
		"You are running as a coding agent in the Codex CLI",
		"the primary agent in a team of agents collaborating to fulfill the user's goals",
	}},
	{Name: "GitHub Copilot", Phrases: []string{
		"Your name is GitHub Copilot",
		"you must respond with \"GitHub Copilot\"",
		"You are an expert AI programming assistant, working with a user in the VS Code editor",
		"You are a highly sophisticated automated coding agent with expert-level knowledge across many different programming languages and frameworks",
	}},
	{Name: "Gemini CLI", Phrases: []string{
		"You are Gemini CLI,",
	}},
	{Name: "Qwen Code", Phrases: []string{
		"You are Qwen Code,",
	}},
	{Name: "Cline", Phrases: []string{
		"You are Cline,",
	}},
	{Name: "Roo Code", Phrases: []string{
		"You are Roo,",
	}},
	{Name: "Kimi CLI", Phrases: []string{
		"You are Kimi Code CLI,",
	}},
	{Name: "Crush", Phrases: []string{
		"You are Crush,",
	}},
	{Name: "Goose", Phrases: []string{
		"AI agent called goose",
		"goose is being developed as an open-source software project",
	}},
	{Name: "Aider", Phrases: []string{
		"Take requests for changes to the supplied code",
		"Act as an expert architect engineer and provide direction to your editor engineer",
		"Act as an expert code analyst. Answer questions about the supplied code",
		"Respect and use existing conventions, libraries, etc that are already present in the code base",
	}},
	{Name: "OpenHands", Phrases: []string{
		"You are OpenHands agent,",
	}},
	{Name: "OpenClaw", Phrases: []string{
		"You are a personal assistant running inside OpenClaw",
	}},
	{Name: "Hermes Agent", Phrases: []string{
		"You are Hermes Agent,",
		"You run on Hermes Agent",
	}},
	{Name: "Grok CLI", Phrases: []string{
		"You are Grok released by xAI",
	}},
	{Name: "pi", Phrases: []string{
		"operating inside pi, a coding agent harness",
	}},
	{Name: "oh-my-pi", Phrases: []string{
		"Oh My Pi coding harness",
		"You are omp's trusted coding assistant",
	}},
	{Name: "MiMo Code", Phrases: []string{
		"You are MiMoCode",
	}},
	{Name: "ZCode", Phrases: []string{
		"You are ZCode,",
		"You are an interactive ZCode agent",
	}},
	{Name: "DeepSeek Harness", Phrases: []string{
		"powered by DeepSeek Harness",
	}},
	{Name: "Agent Zero", Phrases: []string{
		"Agent Zero System Manual",
		"agent zero autonomous json ai agent",
	}},
	{Name: "OpenSquilla", Phrases: []string{
		"You are OpenSquilla,",
	}},
	{Name: "Zed", Phrases: []string{
		"You are the Zed coding agent",
	}},
	{Name: "Continue", Phrases: []string{
		"Always include the language and file name in the info string when you write code blocks",
	}},
	{Name: "Void", Phrases: []string{
		"Here is an overview of the user's file system",
	}},
	{Name: "Windsurf", Phrases: []string{
		"You are Cascade,",
		"designed by the Windsurf engineering team",
	}},
	{Name: "Trae", Phrases: []string{
		"You are Trae AI,",
		"You operate exclusively in Trae AI",
	}},
	{Name: "Cursor", Phrases: []string{
		"You operate in Cursor",
		"Bias towards not asking the user for help if you can find the answer yourself",
	}},
	{Name: "Kiro", Phrases: []string{
		"You are Kiro, an AI assistant and IDE built to assist developers",
	}},
	{Name: "Amp", Phrases: []string{
		"You are Amp,",
	}},
	{Name: "Augment Code", Phrases: []string{
		"You are Augment Agent developed by Augment Code",
	}},
	{Name: "Junie", Phrases: []string{
		"Your name is Junie",
	}},
	{Name: "Warp", Phrases: []string{
		"an AI agent running within Warp",
	}},
}
