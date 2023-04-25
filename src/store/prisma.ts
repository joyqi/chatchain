import { PrismaVectorStore } from "langchain/vectorstores/prisma";
import { PrismaClient, Prisma, Document as PrismaDocument } from "@prisma/client";
import { inMemory } from "node-inmemory";
import { getEmbeddings, Store } from ".";
import { Document } from "langchain/document";

const [getDb] = inMemory(() => new PrismaClient());

const [getVectorStore] = inMemory(() => {
    return PrismaVectorStore.withModel<PrismaDocument>(getDb()).create(
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

export default class implements Store {
    async saveVector(docs: Document[]) {
        // truncate table
        await getDb().document.deleteMany({});

        await getVectorStore().addModels(
            await getDb().$transaction(
                docs.map(doc => getDb().document.create({ data: { content: doc.pageContent } }))
            )
        );
    }

    getVectorStore() {
        return getVectorStore().asRetriever();
    }
}