package sysprompt

// signatures were chosen against a corpus of the agents' captured and
// published prompts (see TestCorpus): every phrase occurs in its agent's
// default prompts and in no other agent's, nor in a side request.
var signatures = compile([]signature{
	{"OpenCode", []string{
		"You are opencode,",
		"Your name is opencode",
		"You are an AI agent running in OpenCode, a coding agent harness",
		"users should report the issue at https://github.com/anomalyco/opencode",
	}},
	{"Kilo Code", []string{
		"You are Kilo,",
		"You are Kilo Code,",
		"users should report the issue at https://github.com/Kilo-Org/kilocode",
	}},
	{"Claude Code", []string{
		"Anthropic's official CLI for Claude",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK",
		"You are running inside the Claude desktop app",
	}},
	{"Codex", []string{
		"You are Codex, ",
		"You are a coding agent running in the Codex CLI",
		"Codex CLI is an open source project led by OpenAI",
		"You are running as a coding agent in the Codex CLI",
		"the primary agent in a team of agents collaborating to fulfill the user's goals",
	}},
	{"GitHub Copilot", []string{
		"Your name is GitHub Copilot",
		"you must respond with \"GitHub Copilot\"",
		"You are an expert AI programming assistant, working with a user in the VS Code editor",
		"You are a highly sophisticated automated coding agent with expert-level knowledge across many different programming languages and frameworks",
	}},
	{"Gemini CLI", []string{
		"You are Gemini CLI,",
	}},
	{"Qwen Code", []string{
		"You are Qwen Code,",
	}},
	{"Cline", []string{
		"You are Cline,",
	}},
	{"Roo Code", []string{
		"You are Roo,",
	}},
	{"Kimi CLI", []string{
		"You are Kimi Code CLI,",
	}},
	{"Crush", []string{
		"You are Crush,",
	}},
	{"Goose", []string{
		"AI agent called goose",
		"goose is being developed as an open-source software project",
	}},
	{"Aider", []string{
		"Take requests for changes to the supplied code",
		"Act as an expert architect engineer and provide direction to your editor engineer",
		"Act as an expert code analyst. Answer questions about the supplied code",
		"Respect and use existing conventions, libraries, etc that are already present in the code base",
	}},
	{"OpenHands", []string{
		"You are OpenHands agent,",
	}},
	{"OpenClaw", []string{
		"You are a personal assistant running inside OpenClaw",
	}},
	{"Hermes Agent", []string{
		"You are Hermes Agent,",
		"You run on Hermes Agent",
	}},
	{"Grok CLI", []string{
		"You are Grok released by xAI",
	}},
	{"pi", []string{
		"operating inside pi, a coding agent harness",
	}},
	{"oh-my-pi", []string{
		"Oh My Pi coding harness",
		"You are omp's trusted coding assistant",
	}},
	{"MiMo Code", []string{
		"You are MiMoCode",
	}},
	{"ZCode", []string{
		"You are ZCode,",
		"You are an interactive ZCode agent",
	}},
	{"DeepSeek Harness", []string{
		"powered by DeepSeek Harness",
	}},
	{"Agent Zero", []string{
		"Agent Zero System Manual",
		"agent zero autonomous json ai agent",
	}},
	{"OpenSquilla", []string{
		"You are OpenSquilla,",
	}},
	{"Zed", []string{
		"You are the Zed coding agent",
	}},
	{"Continue", []string{
		"Always include the language and file name in the info string when you write code blocks",
	}},
	{"Void", []string{
		"Here is an overview of the user's file system",
	}},
	{"Windsurf", []string{
		"You are Cascade,",
		"designed by the Windsurf engineering team",
	}},
	{"Trae", []string{
		"You are Trae AI,",
		"You operate exclusively in Trae AI",
	}},
	{"Cursor", []string{
		"You operate in Cursor",
		"Bias towards not asking the user for help if you can find the answer yourself",
	}},
	{"Kiro", []string{
		"You are Kiro, an AI assistant and IDE built to assist developers",
	}},
	{"Amp", []string{
		"You are Amp,",
	}},
	{"Augment Code", []string{
		"You are Augment Agent developed by Augment Code",
	}},
	{"Junie", []string{
		"Your name is Junie",
	}},
	{"Warp", []string{
		"an AI agent running within Warp",
	}},
})
