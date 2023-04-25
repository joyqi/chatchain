# Read GPT

使用 GPT 读取文本并将其向量化存储，用于生成对话场景。

## 原理

将预处理后的文本切割并转换为向量（使用 OpenAI 的 Embedding），然后存储到向量数据库中。在后续的对话中先对问题进行向量化处理，然后在向量数据库中搜索到相关文本。使用这些文本生成对话 prompt 提交至 OpenAI，然后获取对话的结果。

## 使用

### 1. 安装依赖

环境需求：

- Node.js 18.0.0+
- pnpm 6.0.0+
- 安装了 pgvector 插件的 PostgreSQL 数据库(可选，默认使用 HNSWLib 数据库)

在根目录运行如下命令安装所有依赖

```bash
pnpm install
```
### 2. 配置

在项目根目录下放置一个 `.env` 文件，内容如下：

```
LLM_API_KEY=YOUR_API_KEY
```

把这里的 `YOUR_API_KEY` 改为你的大语言模型 API Key(默认是 OpenAI)。

如果你想使用 http 代理，可以在 `.env` 文件中添加如下内容：

```
HTTP_PROXY=YOUR_PROXY_URL
HTTPS_PROXY=YOUR_PROXY_URL
```

有时候因为你的语料是其它语言，所以输出的结果可能会是其它语言，你可以在 `.env` 文件中添加如下内容：

```
CHAT_LANG=Chinese
```

### 3. Docker 安装 PostgreSQL 数据库(可选)

注意，这一步是可选的，如果你已经安装了 PostgreSQL 数据库或者想直接使用 HNSWLib 数据库，可以跳过这一步。

在 `.env` 文件里添加如下内容：

```bash
POSTGRES_USER=super
POSTGRES_PASSWORD=123456
POSTGRES_DB=test
DATABASE_URL=postgresql://super:123456@127.0.0.1:5432/test
VECTOR_STORE=prisma
```

请把 `POSTGRES_USER` 和 `POSTGRES_PASSWORD` 改为你想要的用户名和密码，`POSTGRES_DB` 改为你想要的数据库名，`DATABASE_URL` 改为你想要的数据库连接地址。然后运行如下命令启动 PostgreSQL 数据库：

```bash
docker-compose up -d
```

然后执行以下命令初始化数据库：

```bash
pnpm exec init:db
```

### 4. 运行

执行以下命令启动服务：

```bash
pnpm exec start
```

