import { OpenAIEmbeddings } from "langchain/embeddings/openai";
import { OpenAI } from "langchain/llms/openai";
import { inMemory } from "node-inmemory";
import { apiKey } from "../config";
import { Document } from "langchain/document";
import { VectorStoreRetriever } from "langchain/vectorstores/base";

export const [getEmbeddings] = inMemory(() => new OpenAIEmbeddings({
    openAIApiKey: apiKey,
}));

export const [getModel] = inMemory(() => new OpenAI({
    openAIApiKey: apiKey,
}));

export interface Store {
    saveVector(docs: Document[]): Promise<void>;
    getVectorStore(): VectorStoreRetriever;
}

import PrismaStore from "./prisma";
import HNSWLibStore from "./hnswlib";

export { PrismaStore, HNSWLibStore };