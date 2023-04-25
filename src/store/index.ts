import { OpenAIEmbeddings } from "langchain/embeddings/openai";
import { inMemory } from "node-inmemory";
import { llmKey } from "../config";
import { Document } from "langchain/document";
import { VectorStoreRetriever } from "langchain/vectorstores/base";

export const [getEmbeddings] = inMemory(() => new OpenAIEmbeddings({
    openAIApiKey: llmKey,
}));

export interface Store {
    saveVector(docs: Document[]): Promise<void>;
    getVectorStore(): VectorStoreRetriever;
}

import PrismaStore from "./prisma";
import HNSWLibStore from "./hnswlib";

export { PrismaStore, HNSWLibStore };