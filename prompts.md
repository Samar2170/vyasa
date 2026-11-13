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


#### Metrics
* we need to write a functionality where key metrics are calculated by year from data present in tickertape_balancesheet_annual, tickertape_income_annual, tickertape_cashflow_annual. raw figures present in those tables will also be copied. metrics table to include

```python
['fy',
 'end_date',
 'data_gaps',
 'total_revenue',
 'gross_profit',
 'ebitda',
 'ebit',
 'pbt',
 'net_income',
 'eps',
 'cfo',
 'capex',
 'fcf',
 'total_equity',
 'minority_interest',
 'net_worth',
 'total_debt',
 'cash_and_sti',
 'total_assets',
 'current_assets',
 'current_liabilities',
 'receivables',
 'inventory',
 'payables',
 'shares_outstanding',
 'revenue_yoy',
 'revenue_cagr_3y',
 'revenue_cagr_5y',
 'ebitda_yoy',
 'ebitda_cagr_3y',
 'ebitda_cagr_5y',
 'ebit_yoy',
 'ebit_cagr_3y',
 'ebit_cagr_5y',
 'pat_yoy',
 'pat_cagr_3y',
 'pat_cagr_5y',
 'eps_yoy',
 'eps_cagr_3y',
 'eps_cagr_5y',
 'dol_ebit',
 'dol_ebitda',
 'gross',
 'ebitda',
 'ebit',
 'net',
 'roe',
 'ronw',
 'roce',
 'roic',
 'debt_to_equity',
 'net_debt',
 'net_debt_to_ebitda',
 'interest_coverage',
 'current_ratio',
 'dso_days',
 'dio_days',
 'dpo_days',
 'wc_cycle_days',
 'receivables_yoy',
 'receivables_pct_revenue',
 'inventory_yoy',
 'inventory_pct_revenue',
 'payables_yoy',
 'payables_pct_cogs',
 'cfo',
 'capex',
 'fcf',
 'fcf_margin',
 'cfo_to_pat',
 'fcf_to_pat',
 'cfo_to_ebitda']```
    