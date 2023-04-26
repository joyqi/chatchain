# Chat Chain

Use LangChain to read text and vectorize it for generating dialogue scenarios.

## Principle

Split and convert the preprocessed text into vectors (default using OpenAI's Embedding), then store it in the vector database. In the subsequent dialogue, first vectorize the question, then search for relevant text in the vector database. Use these texts to generate dialogue prompts and submit them to LLM for results.

## Usage

### 1. Install dependencies

Requirements:

- Node.js 18.0.0+
- pnpm 6.0.0+
- PostgreSQL database with pgvector plugin installed (optional, HNSWLib database is used by default)

Run the following command to install all dependencies in the root directory

```bash
pnpm install
```

### 2. Configuration

Place a `.env` file in the project root directory with the following content:

```
LLM_API_KEY=YOUR_API_KEY
```

Replace `YOUR_API_KEY` with your LLM API Key (default is OpenAI).

If you want to use an http proxy, you can add the following content to the `.env` file:

```
HTTP_PROXY=YOUR_PROXY_URL
HTTPS_PROXY=YOUR_PROXY_URL
```

Sometimes, because your corpus is in another language, the output result may be in another language. You can add the following content to the `.env` file:

```
LLM_LANG=Chinese
```

### 3. Docker install PostgreSQL database (optional)

Note that this step is optional. If you have already installed the PostgreSQL database or want to use the HNSWLib database directly, you can skip this step.

Add the following content to the `.env` file:

```bash
POSTGRES_USER=super
POSTGRES_PASSWORD=123456
POSTGRES_DB=test
DATABASE_URL=postgresql://super:123456@127.0.0.1:5432/test
VECTOR_STORE=prisma
```

Replace the `POSTGRES_USER`, `POSTGRES_PASSWORD` and `POSTGRES_DB` with your own values. The `DATABASE_URL` is the database connection URL, which is composed of the `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB` and the database address. The `VECTOR_STORE` is the vector database type, which can be `hnswlib` or `prisma`. Run the following command to start the database:

```bash
docker-compose up -d
```

Run the following command to install the pgvector plugin:

```bash
pnpm exec init:db
```

### 4. Start the server

```bash
pnpm start
```