#### Arch

I'm building a workflow framework
Written in go

Thinking of these modules
* LLM (Provider interfaces)
* Agent (Core agent loop, step, base for specialist agents)
* Generic tools
* Indexer (chunk text data, embedd, save to vectors with some metadata of source)
* DB, S3 clients
* Logging and action persistence to db (LLM reasoning, response, tool calls, prompts for each session) (maybe even save tool call results)
* Sessions

* Workflow [each can contain below mentioned modules]
    * Facts (Data pipeline acting as source or part of initial context)
    * Special tools
    * Orchestrator
    * Its own db saves