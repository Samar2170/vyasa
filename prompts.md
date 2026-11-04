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



#### Indexer
1. We need to implement a RAG for our ir fetched reports. 
2. We should implement a base indexer, that chunks it up, gets vector embeddings, stores it later for retrieval. (chunking function should be pluggable)
3. We will use a new table
4. 2 custom chunking functions for both annual reports & investor presentation
5. For retrieval we will use semantic hybrid vector & lexical search. 
