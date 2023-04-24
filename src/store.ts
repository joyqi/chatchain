import { PrismaVectorStore } from "langchain/vectorstores/prisma";
import { OpenAIEmbeddings } from "langchain/embeddings/openai";
import { PrismaClient, Prisma, Document } from "@prisma/client";
import { inMemory } from "node-inmemory";
import { apiKey } from "./args";

const [getDb] = inMemory(() => new PrismaClient());

const [getEmbeddings] = inMemory(() => new OpenAIEmbeddings({
    openAIApiKey: apiKey,
}));

export const [getVectorStore] = inMemory(() => {
    return PrismaVectorStore.withModel<Document>(getDb()).create(
        getEmbeddings(),
        {
            prisma: Prisma,
            tableName: "Document",
            vectorColumnName: "vector",
            columns: {
                id: PrismaVectorStore.IdColumn,
                content: PrismaVectorStore.ContentColumn,
            }
        }
    );
});

export async function saveVector(texts: string[]) {
    await getVectorStore().addModels(
        await getDb().$transaction(
            texts.map(content => getDb().document.create({ data: { content } }))
        )
    );
}